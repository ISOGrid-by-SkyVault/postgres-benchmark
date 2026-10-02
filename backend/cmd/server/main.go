// Command server is the benchmark API.
//
// It is built to fail loudly. Any condition it cannot work under ends the
// process with a non-zero exit status, so the container runtime, and the
// platform above it, sees the failure instead of a process that is up but
// useless:
//
//   - invalid or missing configuration: exit at startup
//   - results or target database unreachable at startup: exit
//   - HTTP server cannot listen: exit
//   - results database unreachable for too long while running: /healthz turns
//     503 first, then the process exits
//
// `server healthcheck` probes /healthz of a running instance. The container
// image has no shell or curl, so the binary is its own health probe.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/api"
	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/bench"
	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/config"
	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/store"
	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/target"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	// SIGTERM is how Docker and orchestrators ask the process to stop.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	st, err := store.Open(startup, cfg.ResultsDatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if n, err := st.FailInterrupted(startup); err != nil {
		return fmt.Errorf("results database: %w", err)
	} else if n > 0 {
		log.Warn("marked interrupted runs as failed", "runs", n)
	}

	tg, err := target.Connect(startup, cfg.TargetLabel, cfg.TargetDatabaseURL, cfg.TargetReplicaURLs, cfg.TargetMaxConnections)
	if err != nil {
		return err
	}
	defer tg.Close()

	topo, err := tg.DetectTopology(startup)
	if err != nil {
		return err
	}
	log.Info("target database ready", "label", cfg.TargetLabel, "topology", topo.Mode, "nodes", topo.NodeCount,
		"version", topo.ServerVersion, "streamingReplicas", topo.StreamingReplicas, "readEndpoints", topo.ReadEndpoints)

	runner := bench.NewRunner(st, tg, log)
	health := &watchdog{}
	health.healthy.Store(true)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.New(st, tg, runner, health, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Either goroutine reports the reason the process must stop.
	fatal := make(chan error, 2)
	go func() {
		log.Info("listening", "port", cfg.Port)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			fatal <- fmt.Errorf("http server: %w", err)
		}
	}()
	go func() {
		fatal <- health.watch(ctx, st, cfg.HealthInterval, cfg.HealthFailureThreshold, log)
	}()

	var failure error
	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case failure = <-fatal:
	}

	// Stop accepting requests, then let the run in progress record its state
	// and drop its schema.
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	_ = server.Shutdown(shutdown)
	runner.Shutdown()

	return failure
}

// watchdog tracks whether the results database answers. Without it the
// application cannot record or serve anything, so a lasting outage is fatal.
type watchdog struct {
	healthy atomic.Bool
}

func (w *watchdog) Healthy() bool { return w.healthy.Load() }

// watch pings the results database on every tick. It returns an error once
// `threshold` consecutive pings have failed, and nil when ctx ends.
func (w *watchdog) watch(ctx context.Context, st *store.Store, every time.Duration, threshold int, log *slog.Logger) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		pingCtx, cancel := context.WithTimeout(ctx, min(every, 5*time.Second))
		err := st.Ping(pingCtx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}

		if err == nil {
			if failures > 0 {
				log.Info("results database is reachable again")
			}
			failures = 0
			w.healthy.Store(true)
			continue
		}

		failures++
		w.healthy.Store(false)
		log.Error("results database check failed", "consecutiveFailures", failures, "threshold", threshold, "error", err.Error())
		if failures >= threshold {
			return fmt.Errorf("results database unreachable for %d consecutive checks: %w", failures, err)
		}
	}
}

// healthcheck returns 0 when the local server answers 200 on /healthz.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
