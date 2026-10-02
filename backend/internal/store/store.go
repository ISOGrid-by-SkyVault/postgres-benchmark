// Package store persists benchmark runs and their results in the results
// database, a small PostgreSQL database separate from the one under test.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotFound is returned when a run does not exist.
var ErrNotFound = errors.New("run not found")

// Run statuses.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Run is one execution of the benchmark suite against the target database.
type Run struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	TargetLabel string `json:"targetLabel"`
	// Topology is "single" or "multi-node", as detected when the run started.
	Topology      string `json:"topology"`
	NodeCount     int    `json:"nodeCount"`
	ServerVersion string `json:"serverVersion"`
	// Parameters the run was started with.
	Scale           int `json:"scale"`
	Concurrency     int `json:"concurrency"`
	DurationSeconds int `json:"durationSeconds"`
	// Progress, updated while the run executes.
	CurrentAction    string     `json:"currentAction"`
	CompletedActions int        `json:"completedActions"`
	TotalActions     int        `json:"totalActions"`
	Error            string     `json:"error"`
	StartedAt        time.Time  `json:"startedAt"`
	FinishedAt       *time.Time `json:"finishedAt"`
}

// Result is the measurement of one benchmarked action within a run.
type Result struct {
	Position int    `json:"position"`
	Action   string `json:"action"`
	Title    string `json:"title"`
	Category string `json:"category"`
	// Node is where the action ran: "primary" or "replica".
	Node string `json:"node"`
	// Kind is "timed" (many operations, latency percentiles are meaningful)
	// or "once" (a single operation, only the duration is meaningful).
	Kind         string  `json:"kind"`
	Operations   int64   `json:"operations"`
	Errors       int64   `json:"errors"`
	Rows         int64   `json:"rows"`
	DurationMs   float64 `json:"durationMs"`
	OpsPerSecond float64 `json:"opsPerSecond"`
	LatencyAvgMs float64 `json:"latencyAvgMs"`
	LatencyP50Ms float64 `json:"latencyP50Ms"`
	LatencyP95Ms float64 `json:"latencyP95Ms"`
	LatencyP99Ms float64 `json:"latencyP99Ms"`
	LatencyMinMs float64 `json:"latencyMinMs"`
	LatencyMaxMs float64 `json:"latencyMaxMs"`
	ErrorSample  string  `json:"errorSample"`
}

// Store is the results database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the results database and creates its tables if needed.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("results database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("results database: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("results database schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks that the results database answers.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// FailInterrupted marks runs left "running" by a previous process as failed.
// A run cannot survive a restart, so anything still running at startup was cut
// short.
func (s *Store) FailInterrupted(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE benchmark_runs
		SET status = $1, error = 'interrupted: the server stopped during the run', finished_at = now()
		WHERE status = $2`, StatusFailed, StatusRunning)
	return tag.RowsAffected(), err
}

const runColumns = `id::text, status, target_label, topology, node_count, server_version,
	scale, concurrency, duration_seconds, current_action, completed_actions, total_actions,
	error, started_at, finished_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Status, &r.TargetLabel, &r.Topology, &r.NodeCount, &r.ServerVersion,
		&r.Scale, &r.Concurrency, &r.DurationSeconds, &r.CurrentAction, &r.CompletedActions, &r.TotalActions,
		&r.Error, &r.StartedAt, &r.FinishedAt)
	return r, err
}

// CreateRun inserts a new run in the "running" state and returns it with its
// generated ID.
func (s *Store) CreateRun(ctx context.Context, r Run) (Run, error) {
	return scanRun(s.pool.QueryRow(ctx, `
		INSERT INTO benchmark_runs
			(status, target_label, topology, node_count, server_version, scale, concurrency, duration_seconds, total_actions)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+runColumns,
		StatusRunning, r.TargetLabel, r.Topology, r.NodeCount, r.ServerVersion,
		r.Scale, r.Concurrency, r.DurationSeconds, r.TotalActions))
}

// SetProgress records which action is executing and how many are done.
func (s *Store) SetProgress(ctx context.Context, runID, action string, completed int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE benchmark_runs SET current_action = $2, completed_actions = $3 WHERE id = $1::uuid`,
		runID, action, completed)
	return err
}

// AddResult saves the measurement of one action.
func (s *Store) AddResult(ctx context.Context, runID string, r Result) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO benchmark_results
			(run_id, position, action, title, category, node, kind, operations, errors, rows_affected,
			 duration_ms, ops_per_second, latency_avg_ms, latency_p50_ms, latency_p95_ms, latency_p99_ms,
			 latency_min_ms, latency_max_ms, error_sample)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		runID, r.Position, r.Action, r.Title, r.Category, r.Node, r.Kind, r.Operations, r.Errors, r.Rows,
		r.DurationMs, r.OpsPerSecond, r.LatencyAvgMs, r.LatencyP50Ms, r.LatencyP95Ms, r.LatencyP99Ms,
		r.LatencyMinMs, r.LatencyMaxMs, r.ErrorSample)
	return err
}

// FinishRun moves a run to its final status.
func (s *Store) FinishRun(ctx context.Context, runID, status, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE benchmark_runs
		SET status = $2, error = $3, current_action = '', finished_at = now(),
		    completed_actions = CASE WHEN $2 = 'completed' THEN total_actions ELSE completed_actions END
		WHERE id = $1::uuid`, runID, status, errMsg)
	return err
}

// ListRuns returns the most recent runs, newest first.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runColumns+` FROM benchmark_runs ORDER BY started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// GetRun returns a run with the results recorded so far.
func (s *Store) GetRun(ctx context.Context, runID string) (Run, []Result, error) {
	run, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM benchmark_runs WHERE id = $1::uuid`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, nil, ErrNotFound
	}
	if err != nil {
		return Run{}, nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT position, action, title, category, node, kind, operations, errors, rows_affected,
		       duration_ms, ops_per_second, latency_avg_ms, latency_p50_ms, latency_p95_ms, latency_p99_ms,
		       latency_min_ms, latency_max_ms, error_sample
		FROM benchmark_results WHERE run_id = $1::uuid ORDER BY position`, runID)
	if err != nil {
		return Run{}, nil, err
	}
	defer rows.Close()

	results := []Result{}
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Position, &r.Action, &r.Title, &r.Category, &r.Node, &r.Kind, &r.Operations,
			&r.Errors, &r.Rows, &r.DurationMs, &r.OpsPerSecond, &r.LatencyAvgMs, &r.LatencyP50Ms,
			&r.LatencyP95Ms, &r.LatencyP99Ms, &r.LatencyMinMs, &r.LatencyMaxMs, &r.ErrorSample); err != nil {
			return Run{}, nil, err
		}
		results = append(results, r)
	}
	return run, results, rows.Err()
}

// DeleteRun removes a finished run and its results. A running run is left
// alone and reported as not found.
func (s *Store) DeleteRun(ctx context.Context, runID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM benchmark_runs WHERE id = $1::uuid AND status <> $2`, runID, StatusRunning)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
