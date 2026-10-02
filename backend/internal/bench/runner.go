// Package bench runs the benchmark suite against the target database.
//
// A run creates its own schema on the target, executes every action in
// order, saves one result per action in the results database and drops the
// schema again. Only one run executes at a time.
package bench

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/store"
	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/target"
)

// ErrBusy is returned when a run is requested while another one executes.
var ErrBusy = errors.New("a benchmark run is already in progress")

// Params are the knobs of a run.
type Params struct {
	// Scale is the number of customers loaded. Products, orders and order
	// items are derived from it.
	Scale int `json:"scale"`
	// Concurrency is the number of parallel workers of each timed action.
	Concurrency int `json:"concurrency"`
	// DurationSeconds is how long each timed action runs.
	DurationSeconds int `json:"durationSeconds"`
}

// Limits of the parameters.
const (
	MinScale    = 1_000
	MaxScale    = 200_000
	MinDuration = 1
	MaxDuration = 30
)

// Validate fills in defaults and rejects out-of-range values.
func (p *Params) Validate(maxConcurrency int) error {
	if p.Scale == 0 {
		p.Scale = 10_000
	}
	if p.Concurrency == 0 {
		p.Concurrency = min(8, maxConcurrency)
	}
	if p.DurationSeconds == 0 {
		p.DurationSeconds = 3
	}
	if p.Scale < MinScale || p.Scale > MaxScale {
		return fmt.Errorf("scale must be between %d and %d", MinScale, MaxScale)
	}
	if p.Concurrency < 1 || p.Concurrency > maxConcurrency {
		return fmt.Errorf("concurrency must be between 1 and %d", maxConcurrency)
	}
	if p.DurationSeconds < MinDuration || p.DurationSeconds > MaxDuration {
		return fmt.Errorf("durationSeconds must be between %d and %d", MinDuration, MaxDuration)
	}
	return nil
}

// Runner starts runs and tracks the one in progress.
type Runner struct {
	store  *store.Store
	target *target.Target
	log    *slog.Logger

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    sync.WaitGroup
}

// NewRunner builds a Runner.
func NewRunner(st *store.Store, tg *target.Target, log *slog.Logger) *Runner {
	return &Runner{store: st, target: tg, log: log}
}

// Running reports whether a run is executing.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Start validates the parameters, records a new run and executes it in the
// background. It returns as soon as the run is recorded.
func (r *Runner) Start(ctx context.Context, p Params) (store.Run, error) {
	if err := p.Validate(r.target.MaxConcurrency()); err != nil {
		return store.Run{}, &ParamError{err}
	}

	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return store.Run{}, ErrBusy
	}
	r.running = true
	r.mu.Unlock()

	release := func() {
		r.mu.Lock()
		r.running = false
		r.cancel = nil
		r.mu.Unlock()
	}

	topo, err := r.target.DetectTopology(ctx)
	if err != nil {
		release()
		return store.Run{}, err
	}

	rc := newRunContext(r.target, p)
	actions := suite(rc)

	run, err := r.store.CreateRun(ctx, store.Run{
		TargetLabel:     r.target.Label,
		Topology:        topo.Mode,
		NodeCount:       topo.NodeCount,
		ServerVersion:   topo.ServerVersion,
		Scale:           p.Scale,
		Concurrency:     p.Concurrency,
		DurationSeconds: p.DurationSeconds,
		TotalActions:    len(actions),
	})
	if err != nil {
		release()
		return store.Run{}, err
	}
	rc.useSchema(run.ID)

	// The run outlives the HTTP request that started it.
	runCtx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()

	r.done.Add(1)
	go func() {
		defer r.done.Done()
		defer release()
		defer cancel()
		r.execute(runCtx, run, rc, actions)
	}()

	return run, nil
}

// Shutdown cancels the run in progress, if any, and waits for it to record
// its failure and clean up.
func (r *Runner) Shutdown() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.done.Wait()
}

func (r *Runner) execute(ctx context.Context, run store.Run, rc *runContext, actions []Action) {
	log := r.log.With("run", run.ID)
	log.Info("run started", "topology", run.Topology, "nodes", run.NodeCount,
		"scale", run.Scale, "concurrency", run.Concurrency, "durationSeconds", run.DurationSeconds)

	// Bookkeeping and cleanup must still work after the run is cancelled.
	bookkeeping := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 30*time.Second)
	}

	// Whatever happens, leave nothing behind on the target.
	defer func() {
		cleanupCtx, cancel := bookkeeping()
		defer cancel()
		if _, err := r.target.Primary.Exec(cleanupCtx, rc.q(`DROP SCHEMA IF EXISTS {s} CASCADE`)); err != nil {
			log.Error("could not drop the benchmark schema", "schema", rc.schema, "error", err)
		}
	}()

	fail := func(action string, err error) {
		msg := fmt.Sprintf("%s: %v", action, err)
		if ctx.Err() != nil {
			msg = "cancelled: the server is shutting down"
		}
		log.Error("run failed", "error", msg)
		bctx, cancel := bookkeeping()
		defer cancel()
		if err := r.store.FinishRun(bctx, run.ID, store.StatusFailed, msg); err != nil {
			log.Error("could not record the failure", "error", err)
		}
	}

	for i, action := range actions {
		if err := r.store.SetProgress(ctx, run.ID, action.Title, i); err != nil {
			fail(action.Title, err)
			return
		}

		m, err := action.Run(ctx, rc)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			fail(action.Title, err)
			return
		}

		s := Summarize(m)
		kind := "once"
		if m.Timed {
			kind = "timed"
		}
		result := store.Result{
			Position: i + 1, Action: action.Key, Title: action.Title, Category: action.Category,
			Node: action.Node, Kind: kind,
			Operations: m.Operations, Errors: m.Errors, Rows: m.Rows,
			DurationMs: s.DurationMs, OpsPerSecond: s.OpsPerSecond,
			LatencyAvgMs: s.AvgMs, LatencyP50Ms: s.P50Ms, LatencyP95Ms: s.P95Ms, LatencyP99Ms: s.P99Ms,
			LatencyMinMs: s.MinMs, LatencyMaxMs: s.MaxMs, ErrorSample: m.ErrorSample,
		}
		if err := r.store.AddResult(ctx, run.ID, result); err != nil {
			fail(action.Title, err)
			return
		}
		log.Info("action finished", "action", action.Key, "operations", m.Operations,
			"opsPerSecond", int64(s.OpsPerSecond), "p95Ms", s.P95Ms, "errors", m.Errors)
	}

	bctx, cancel := bookkeeping()
	defer cancel()
	if err := r.store.FinishRun(bctx, run.ID, store.StatusCompleted, ""); err != nil {
		log.Error("could not record the completion", "error", err)
		return
	}
	log.Info("run completed")
}

// ParamError marks an error caused by invalid run parameters.
type ParamError struct{ Err error }

func (e *ParamError) Error() string { return e.Err.Error() }
func (e *ParamError) Unwrap() error { return e.Err }

// runContext is the state shared by the actions of one run.
type runContext struct {
	target *target.Target
	params Params
	// schema is the name of the schema this run works in; quoted is the same
	// name ready to be placed in SQL.
	schema string
	quoted string

	// Row counts loaded by the bulk load. Later actions pick random IDs in
	// these ranges.
	customers int64
	products  int64
	orders    int64
	items     int64
}

func newRunContext(tg *target.Target, p Params) *runContext {
	customers := int64(p.Scale)
	orders := customers * 5
	return &runContext{
		target:    tg,
		params:    p,
		customers: customers,
		products:  max(customers/10, 100),
		orders:    orders,
		items:     orders * 2,
	}
}

// useSchema derives the schema name from the run ID, so two runs can never
// collide and a leftover schema can be traced back to its run.
func (rc *runContext) useSchema(runID string) {
	rc.schema = "bench_" + strings.ReplaceAll(runID, "-", "")[:12]
	rc.quoted = pgx.Identifier{rc.schema}.Sanitize()
}

// q replaces the {s} placeholder with the run's schema.
func (rc *runContext) q(sql string) string {
	return strings.ReplaceAll(sql, "{s}", rc.quoted)
}

func (rc *runContext) duration() time.Duration {
	return time.Duration(rc.params.DurationSeconds) * time.Second
}
