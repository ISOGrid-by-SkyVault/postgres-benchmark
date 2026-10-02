package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Action categories.
const (
	CategorySchema      = "schema"
	CategoryWrite       = "write"
	CategoryRead        = "read"
	CategoryJoin        = "join"
	CategoryTransaction = "transaction"
	CategoryReplication = "replication"
)

// Action is one benchmarked operation. Each action measures exactly one
// thing, so its result can be compared between runs and between topologies.
type Action struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Description string `json:"description"`
	// Node is where the action runs: "primary" or "replica".
	Node string `json:"node"`

	Run func(ctx context.Context, rc *runContext) (Measurement, error) `json:"-"`
}

// Catalog lists the actions a run executes, for display. Replication actions
// are only part of the suite when read replicas are configured.
func Catalog(hasReplicas bool) []Action {
	readNode := "primary"
	if hasReplicas {
		readNode = "replica"
	}

	actions := []Action{
		{
			Key: "schema_create", Title: "Create schema", Category: CategorySchema, Node: "primary",
			Description: "Creates the schema and five tables linked by foreign keys.",
			Run:         createSchema,
		},
		{
			Key: "bulk_load", Title: "Bulk load (COPY)", Category: CategoryWrite, Node: "primary",
			Description: "Loads customers, products, orders and order items with COPY. Throughput is rows per second.",
			Run:         bulkLoad,
		},
		{
			Key: "index_build", Title: "Build indexes", Category: CategorySchema, Node: "primary",
			Description: "Creates six secondary indexes on the loaded tables, then runs ANALYZE.",
			Run:         buildIndexes,
		},
	}

	if hasReplicas {
		actions = append(actions, Action{
			Key: "replica_catchup", Title: "Replica catch-up", Category: CategoryReplication, Node: "replica",
			Description: "Time for every replica to replay the bulk load and the index builds.",
			Run:         replicaCatchUp,
		})
	}

	actions = append(actions,
		Action{
			Key: "point_select", Title: "Point lookup", Category: CategoryRead, Node: readNode,
			Description: "Fetches one customer by primary key.",
			Run:         pointSelect,
		},
		Action{
			Key: "range_scan", Title: "Range scan", Category: CategoryRead, Node: readNode,
			Description: "Reads up to 100 orders from a six-hour window through the created_at index.",
			Run:         rangeScan,
		},
		Action{
			Key: "join_two_tables", Title: "Two-table join", Category: CategoryJoin, Node: readNode,
			Description: "Joins orders to customers to list one customer's latest orders.",
			Run:         joinTwoTables,
		},
		Action{
			Key: "join_four_tables", Title: "Four-table join with aggregation", Category: CategoryJoin, Node: readNode,
			Description: "Joins customers, orders, order items and products to compute revenue per category for one country.",
			Run:         joinFourTables,
		},
		Action{
			Key: "window_ranking", Title: "CTE with window functions", Category: CategoryJoin, Node: readNode,
			Description: "Ranks the customers of a country by total spend and computes each one's share of the total.",
			Run:         windowRanking,
		},
		Action{
			Key: "anti_join", Title: "Anti-join (NOT EXISTS)", Category: CategoryJoin, Node: readNode,
			Description: "Finds the products of a category that nobody ordered in the last week.",
			Run:         antiJoin,
		},
		Action{
			Key: "insert_single", Title: "Single-row insert", Category: CategoryWrite, Node: "primary",
			Description: "Inserts one order per statement, each in its own transaction.",
			Run:         insertSingle,
		},
		Action{
			Key: "insert_batch", Title: "Batch insert (100 rows)", Category: CategoryWrite, Node: "primary",
			Description: "Inserts 100 order items per statement.",
			Run:         insertBatch,
		},
		Action{
			Key: "update_row", Title: "Update by primary key", Category: CategoryWrite, Node: "primary",
			Description: "Changes the status of one random order.",
			Run:         updateRow,
		},
		Action{
			Key: "order_transaction", Title: "Multi-statement transaction", Category: CategoryTransaction, Node: "primary",
			Description: "Places an order: locks a product row, decrements its stock, inserts the order and its item, commits.",
			Run:         orderTransaction,
		},
		Action{
			Key: "delete_row", Title: "Delete by primary key", Category: CategoryWrite, Node: "primary",
			Description: "Deletes one random order item.",
			Run:         deleteRow,
		},
	)

	if hasReplicas {
		actions = append(actions, Action{
			Key: "replication_lag", Title: "Replication lag", Category: CategoryReplication, Node: "replica",
			Description: "Commits a row on the primary and measures how long until a replica can read it.",
			Run:         replicationLag,
		})
	}

	return append(actions, Action{
		Key: "schema_drop", Title: "Drop schema", Category: CategorySchema, Node: "primary",
		Description: "Drops the schema with everything in it.",
		Run:         dropSchema,
	})
}

func suite(rc *runContext) []Action {
	return Catalog(rc.target.HasReplicas())
}

// Reference data for the generated rows.
var (
	countries  = []string{"DZ", "FR", "DE", "ES", "IT", "GB", "US", "CA", "BR", "MA", "TN", "EG", "TR", "IN", "JP", "NL", "SE", "PL", "PT", "AE"}
	categories = []string{"books", "electronics", "garden", "grocery", "home", "music", "office", "sports", "toys", "travel"}
	statuses   = []string{"pending", "paid", "shipped", "delivered", "cancelled"}
)

// ----------------------------------------------------------------- helpers

// once measures a single operation. fn reports how many units of work it did
// (statements, rows) so a throughput can still be derived.
func once(ctx context.Context, fn func(ctx context.Context) (ops, rows int64, err error)) (Measurement, error) {
	start := time.Now()
	ops, rows, err := fn(ctx)
	return Measurement{Operations: ops, Rows: rows, Wall: time.Since(start)}, err
}

// operation is the unit a timed action repeats. It returns the number of rows
// it read or changed.
type operation func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (rows int64, err error)

// timed runs op from `concurrency` workers for the run's duration and records
// the latency of every successful call.
func (rc *runContext) timed(ctx context.Context, pick func(worker int) *pgxpool.Pool, op operation) (Measurement, error) {
	deadline, cancel := context.WithTimeout(ctx, rc.duration())
	defer cancel()

	type workerResult struct {
		latencies []time.Duration
		errors    int64
		rows      int64
		firstErr  string
	}
	results := make([]workerResult, rc.params.Concurrency)

	var wg sync.WaitGroup
	start := time.Now()
	for w := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w)+1, uint64(start.UnixNano())))
			db := pick(w)
			out := &results[w]

			for deadline.Err() == nil {
				begin := time.Now()
				rows, err := op(deadline, db, rng)
				if err != nil {
					if deadline.Err() != nil {
						break // cut short by the deadline, not a failure
					}
					out.errors++
					if out.firstErr == "" {
						out.firstErr = err.Error()
					}
					time.Sleep(5 * time.Millisecond) // do not spin on a broken connection
					continue
				}
				out.latencies = append(out.latencies, time.Since(begin))
				out.rows += rows
			}
		}()
	}
	wg.Wait()

	m := Measurement{Timed: true, Wall: time.Since(start)}
	for _, r := range results {
		m.Latencies = append(m.Latencies, r.latencies...)
		m.Errors += r.errors
		m.Rows += r.rows
		if m.ErrorSample == "" {
			m.ErrorSample = r.firstErr
		}
	}
	m.Operations = int64(len(m.Latencies))

	if err := ctx.Err(); err != nil {
		return m, err
	}
	if m.Operations == 0 {
		if m.ErrorSample != "" {
			return m, fmt.Errorf("no operation succeeded: %s", m.ErrorSample)
		}
		return m, errors.New("no operation finished within the duration")
	}
	return m, nil
}

func (rc *runContext) onPrimary(int) *pgxpool.Pool { return rc.target.Primary }

func (rc *runContext) onReader(worker int) *pgxpool.Pool { return rc.target.Reader(worker) }

// drain consumes a result set and returns its row count.
func drain(rows pgx.Rows, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// randomID returns a value in [1, n].
func randomID(rng *rand.Rand, n int64) int64 { return rng.Int64N(n) + 1 }

func pickOne(rng *rand.Rand, values []string) string { return values[rng.IntN(len(values))] }

// generated is a pgx.CopyFromSource producing n rows from a function.
type generated struct {
	n, i int64
	row  func(i int64) []any
}

func (g *generated) Next() bool             { g.i++; return g.i <= g.n }
func (g *generated) Values() ([]any, error) { return g.row(g.i), nil }
func (g *generated) Err() error             { return nil }

// ------------------------------------------------------------------ schema

func createSchema(ctx context.Context, rc *runContext) (Measurement, error) {
	statements := []string{
		`CREATE SCHEMA {s}`,
		`CREATE TABLE {s}.customers (
			id         bigint PRIMARY KEY,
			name       text        NOT NULL,
			email      text        NOT NULL,
			country    text        NOT NULL,
			created_at timestamptz NOT NULL
		)`,
		`CREATE TABLE {s}.products (
			id       bigint PRIMARY KEY,
			name     text           NOT NULL,
			category text           NOT NULL,
			price    numeric(10, 2) NOT NULL,
			stock    integer        NOT NULL
		)`,
		`CREATE TABLE {s}.orders (
			id          bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			customer_id bigint         NOT NULL REFERENCES {s}.customers (id),
			status      text           NOT NULL,
			total       numeric(12, 2) NOT NULL,
			created_at  timestamptz    NOT NULL
		)`,
		`CREATE TABLE {s}.order_items (
			id         bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			order_id   bigint         NOT NULL REFERENCES {s}.orders (id) ON DELETE CASCADE,
			product_id bigint         NOT NULL REFERENCES {s}.products (id),
			quantity   integer        NOT NULL,
			unit_price numeric(10, 2) NOT NULL
		)`,
		`CREATE TABLE {s}.replication_probe (
			id         bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			written_at timestamptz NOT NULL DEFAULT now()
		)`,
	}
	return once(ctx, func(ctx context.Context) (int64, int64, error) {
		for _, stmt := range statements {
			if _, err := rc.target.Primary.Exec(ctx, rc.q(stmt)); err != nil {
				return 0, 0, err
			}
		}
		return int64(len(statements)), 0, nil
	})
}

func buildIndexes(ctx context.Context, rc *runContext) (Measurement, error) {
	statements := []string{
		`CREATE INDEX ON {s}.customers (country)`,
		`CREATE INDEX ON {s}.products (category)`,
		`CREATE INDEX ON {s}.orders (customer_id)`,
		`CREATE INDEX ON {s}.orders (created_at)`,
		`CREATE INDEX ON {s}.order_items (order_id)`,
		`CREATE INDEX ON {s}.order_items (product_id)`,
		`ANALYZE {s}.customers`,
		`ANALYZE {s}.products`,
		`ANALYZE {s}.orders`,
		`ANALYZE {s}.order_items`,
	}
	return once(ctx, func(ctx context.Context) (int64, int64, error) {
		for _, stmt := range statements {
			if _, err := rc.target.Primary.Exec(ctx, rc.q(stmt)); err != nil {
				return 0, 0, err
			}
		}
		return int64(len(statements)), 0, nil
	})
}

func dropSchema(ctx context.Context, rc *runContext) (Measurement, error) {
	return once(ctx, func(ctx context.Context) (int64, int64, error) {
		_, err := rc.target.Primary.Exec(ctx, rc.q(`DROP SCHEMA {s} CASCADE`))
		return 1, 0, err
	})
}

// ------------------------------------------------------------------- load

func bulkLoad(ctx context.Context, rc *runContext) (Measurement, error) {
	rng := rand.New(rand.NewPCG(42, uint64(rc.params.Scale)))
	now := time.Now().UTC()
	// Spread timestamps over the past year so range queries have something to select.
	randomPast := func() time.Time {
		return now.Add(-time.Duration(rng.Int64N(int64(365 * 24 * time.Hour))))
	}

	tables := []struct {
		name    string
		columns []string
		source  *generated
	}{
		{"customers", []string{"id", "name", "email", "country", "created_at"}, &generated{n: rc.customers, row: func(i int64) []any {
			return []any{i, fmt.Sprintf("Customer %d", i), fmt.Sprintf("customer%d@example.com", i), pickOne(rng, countries), randomPast()}
		}}},
		{"products", []string{"id", "name", "category", "price", "stock"}, &generated{n: rc.products, row: func(i int64) []any {
			return []any{i, fmt.Sprintf("Product %d", i), pickOne(rng, categories), float64(rng.IntN(49_900)+100) / 100, int32(1_000_000)}
		}}},
		// IDs of orders and order items come from the identity sequence, which
		// starts at 1, so they are 1..N like the generated foreign keys expect.
		{"orders", []string{"customer_id", "status", "total", "created_at"}, &generated{n: rc.orders, row: func(int64) []any {
			return []any{randomID(rng, rc.customers), pickOne(rng, statuses), float64(rng.IntN(99_000)+1_000) / 100, randomPast()}
		}}},
		{"order_items", []string{"order_id", "product_id", "quantity", "unit_price"}, &generated{n: rc.items, row: func(int64) []any {
			return []any{randomID(rng, rc.orders), randomID(rng, rc.products), int32(rng.IntN(5) + 1), float64(rng.IntN(49_900)+100) / 100}
		}}},
	}

	return once(ctx, func(ctx context.Context) (int64, int64, error) {
		var total int64
		for _, table := range tables {
			n, err := rc.target.Primary.CopyFrom(ctx, pgx.Identifier{rc.schema, table.name}, table.columns, table.source)
			if err != nil {
				return total, total, fmt.Errorf("%s: %w", table.name, err)
			}
			total += n
		}
		return total, total, nil
	})
}

// ------------------------------------------------------------------- reads

func pointSelect(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`SELECT id, name, email, country, created_at FROM {s}.customers WHERE id = $1`)
	return rc.timed(ctx, rc.onReader, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		return drain(db.Query(ctx, sql, randomID(rng, rc.customers)))
	})
}

func rangeScan(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`
		SELECT id, customer_id, status, total, created_at
		FROM {s}.orders
		WHERE created_at >= $1::timestamptz AND created_at < $1::timestamptz + interval '6 hours'
		ORDER BY created_at
		LIMIT 100`)
	return rc.timed(ctx, rc.onReader, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		from := time.Now().Add(-time.Duration(rng.Int64N(int64(364 * 24 * time.Hour))))
		return drain(db.Query(ctx, sql, from))
	})
}

func joinTwoTables(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`
		SELECT o.id, o.status, o.total, o.created_at, c.name, c.country
		FROM {s}.orders o
		JOIN {s}.customers c ON c.id = o.customer_id
		WHERE o.customer_id = $1
		ORDER BY o.created_at DESC
		LIMIT 50`)
	return rc.timed(ctx, rc.onReader, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		return drain(db.Query(ctx, sql, randomID(rng, rc.customers)))
	})
}

func joinFourTables(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`
		SELECT p.category,
		       count(DISTINCT o.id)              AS orders,
		       sum(oi.quantity)                  AS units,
		       sum(oi.quantity * oi.unit_price)  AS revenue
		FROM {s}.customers c
		JOIN {s}.orders o       ON o.customer_id = c.id
		JOIN {s}.order_items oi ON oi.order_id = o.id
		JOIN {s}.products p     ON p.id = oi.product_id
		WHERE c.country = $1
		  AND o.created_at >= now() - interval '90 days'
		  AND o.status <> 'cancelled'
		GROUP BY p.category
		ORDER BY revenue DESC`)
	return rc.timed(ctx, rc.onReader, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		return drain(db.Query(ctx, sql, pickOne(rng, countries)))
	})
}

func windowRanking(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`
		WITH spend AS (
			SELECT c.id, c.name, sum(o.total) AS total_spent, count(*) AS orders
			FROM {s}.customers c
			JOIN {s}.orders o ON o.customer_id = c.id
			WHERE c.country = $1
			GROUP BY c.id, c.name
		),
		ranked AS (
			SELECT id, name, total_spent, orders,
			       rank() OVER (ORDER BY total_spent DESC) AS position,
			       total_spent / sum(total_spent) OVER ()  AS share
			FROM spend
		)
		SELECT id, name, total_spent, orders, position, share
		FROM ranked
		WHERE position <= 10
		ORDER BY position`)
	return rc.timed(ctx, rc.onReader, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		return drain(db.Query(ctx, sql, pickOne(rng, countries)))
	})
}

func antiJoin(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`
		SELECT p.id, p.name, p.price
		FROM {s}.products p
		WHERE p.category = $1
		  AND NOT EXISTS (
			SELECT 1
			FROM {s}.order_items oi
			JOIN {s}.orders o ON o.id = oi.order_id
			WHERE oi.product_id = p.id
			  AND o.created_at >= now() - interval '7 days'
		  )
		ORDER BY p.id
		LIMIT 50`)
	return rc.timed(ctx, rc.onReader, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		return drain(db.Query(ctx, sql, pickOne(rng, categories)))
	})
}

// ------------------------------------------------------------------ writes

func insertSingle(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`INSERT INTO {s}.orders (customer_id, status, total, created_at) VALUES ($1, 'pending', $2, now())`)
	return rc.timed(ctx, rc.onPrimary, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		tag, err := db.Exec(ctx, sql, randomID(rng, rc.customers), float64(rng.IntN(99_000)+1_000)/100)
		return tag.RowsAffected(), err
	})
}

func insertBatch(ctx context.Context, rc *runContext) (Measurement, error) {
	const batchSize = 100
	sql := rc.q(`
		INSERT INTO {s}.order_items (order_id, product_id, quantity, unit_price)
		SELECT * FROM unnest($1::bigint[], $2::bigint[], $3::integer[], $4::float8[])`)
	return rc.timed(ctx, rc.onPrimary, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		orderIDs := make([]int64, batchSize)
		productIDs := make([]int64, batchSize)
		quantities := make([]int32, batchSize)
		prices := make([]float64, batchSize)
		for i := range batchSize {
			orderIDs[i] = randomID(rng, rc.orders)
			productIDs[i] = randomID(rng, rc.products)
			quantities[i] = int32(rng.IntN(5) + 1)
			prices[i] = float64(rng.IntN(49_900)+100) / 100
		}
		tag, err := db.Exec(ctx, sql, orderIDs, productIDs, quantities, prices)
		return tag.RowsAffected(), err
	})
}

func updateRow(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`UPDATE {s}.orders SET status = $2 WHERE id = $1`)
	return rc.timed(ctx, rc.onPrimary, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		tag, err := db.Exec(ctx, sql, randomID(rng, rc.orders), pickOne(rng, statuses))
		return tag.RowsAffected(), err
	})
}

func deleteRow(ctx context.Context, rc *runContext) (Measurement, error) {
	sql := rc.q(`DELETE FROM {s}.order_items WHERE id = $1`)
	return rc.timed(ctx, rc.onPrimary, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		tag, err := db.Exec(ctx, sql, randomID(rng, rc.items))
		return tag.RowsAffected(), err
	})
}

func orderTransaction(ctx context.Context, rc *runContext) (Measurement, error) {
	lockProduct := rc.q(`SELECT price FROM {s}.products WHERE id = $1 FOR UPDATE`)
	takeStock := rc.q(`UPDATE {s}.products SET stock = stock - $2 WHERE id = $1`)
	addOrder := rc.q(`INSERT INTO {s}.orders (customer_id, status, total, created_at) VALUES ($1, 'paid', $2, now()) RETURNING id`)
	addItem := rc.q(`INSERT INTO {s}.order_items (order_id, product_id, quantity, unit_price) VALUES ($1, $2, $3, $4)`)

	return rc.timed(ctx, rc.onPrimary, func(ctx context.Context, db *pgxpool.Pool, rng *rand.Rand) (int64, error) {
		productID := randomID(rng, rc.products)
		quantity := int32(rng.IntN(5) + 1)

		tx, err := db.Begin(ctx)
		if err != nil {
			return 0, err
		}
		// No-op once the transaction is committed.
		defer tx.Rollback(context.WithoutCancel(ctx))

		var price float64
		if err := tx.QueryRow(ctx, lockProduct, productID).Scan(&price); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, takeStock, productID, quantity); err != nil {
			return 0, err
		}
		var orderID int64
		if err := tx.QueryRow(ctx, addOrder, randomID(rng, rc.customers), price*float64(quantity)).Scan(&orderID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, addItem, orderID, productID, quantity, price); err != nil {
			return 0, err
		}
		return 3, tx.Commit(ctx)
	})
}

// ------------------------------------------------------------- replication

// replicaCatchUp waits until every replica has replayed everything the
// primary has written so far.
func replicaCatchUp(ctx context.Context, rc *runContext) (Measurement, error) {
	return once(ctx, func(ctx context.Context) (int64, int64, error) {
		var lsn string
		if err := rc.target.Primary.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn); err != nil {
			return 0, 0, err
		}

		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		for i, replica := range rc.target.Replicas {
			for {
				var caughtUp bool
				err := replica.QueryRow(waitCtx, `SELECT coalesce(pg_last_wal_replay_lsn() >= $1::pg_lsn, false)`, lsn).Scan(&caughtUp)
				if err != nil {
					return 0, 0, fmt.Errorf("replica %d: %w", i+1, err)
				}
				if caughtUp {
					break
				}
				select {
				case <-waitCtx.Done():
					return 0, 0, fmt.Errorf("replica %d did not catch up within 2 minutes", i+1)
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		return int64(len(rc.target.Replicas)), 0, nil
	})
}

// replicationLag commits a row on the primary and polls a replica until the
// row is visible there. The latency of each probe is the observed lag.
func replicationLag(ctx context.Context, rc *runContext) (Measurement, error) {
	const (
		probes      = 50
		pollEvery   = 500 * time.Microsecond
		probeBudget = 10 * time.Second
	)
	write := rc.q(`INSERT INTO {s}.replication_probe DEFAULT VALUES RETURNING id`)
	read := rc.q(`SELECT 1 FROM {s}.replication_probe WHERE id = $1`)

	m := Measurement{Timed: true}
	start := time.Now()
	for i := range probes {
		replica := rc.target.Replicas[i%len(rc.target.Replicas)]

		var id int64
		if err := rc.target.Primary.QueryRow(ctx, write).Scan(&id); err != nil {
			return m, err
		}
		committed := time.Now()

		for {
			var one int
			err := replica.QueryRow(ctx, read, id).Scan(&one)
			if err == nil {
				m.Latencies = append(m.Latencies, time.Since(committed))
				m.Rows++
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return m, err
			}
			if time.Since(committed) > probeBudget {
				m.Errors++
				if m.ErrorSample == "" {
					m.ErrorSample = fmt.Sprintf("a row was still not visible on the replica after %s", probeBudget)
				}
				break
			}
			time.Sleep(pollEvery)
		}
	}
	m.Wall = time.Since(start)
	m.Operations = int64(len(m.Latencies))
	if m.Operations == 0 {
		return m, errors.New(m.ErrorSample)
	}
	return m, nil
}
