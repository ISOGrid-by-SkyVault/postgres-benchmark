// Package api is the HTTP layer: JSON in, JSON out, no business logic.
//
//	GET    /healthz          liveness, 503 when the results database is down
//	GET    /api/info         target topology, limits and the action catalog
//	POST   /api/runs         start a run
//	GET    /api/runs         recent runs, newest first
//	GET    /api/runs/{id}    one run with its results so far
//	DELETE /api/runs/{id}    delete a finished run
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/bench"
	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/store"
	"github.com/ISOGrid-by-SkyVault/postgres-benchmark/backend/internal/target"
)

// Health reports the state maintained by the watchdog in main.
type Health interface {
	// Healthy is false while the results database does not answer.
	Healthy() bool
}

// Server holds the dependencies of the handlers.
type Server struct {
	store  *store.Store
	target *target.Target
	runner *bench.Runner
	health Health
	log    *slog.Logger
}

// New builds the HTTP handler.
func New(st *store.Store, tg *target.Target, runner *bench.Runner, health Health, log *slog.Logger) http.Handler {
	s := &Server{store: st, target: tg, runner: runner, health: health, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("POST /api/runs", s.startRun)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("DELETE /api/runs/{id}", s.deleteRun)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if !s.health.Healthy() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "resultsDatabase": "down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy", "resultsDatabase": "up"})
}

type infoResponse struct {
	TargetLabel string           `json:"targetLabel"`
	Topology    *target.Topology `json:"topology"`
	// TargetError is set when the target database did not answer.
	TargetError    string         `json:"targetError"`
	Running        bool           `json:"running"`
	MaxConcurrency int            `json:"maxConcurrency"`
	Limits         map[string]int `json:"limits"`
	Actions        []bench.Action `json:"actions"`
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	resp := infoResponse{
		TargetLabel:    s.target.Label,
		Running:        s.runner.Running(),
		MaxConcurrency: s.target.MaxConcurrency(),
		Limits: map[string]int{
			"minScale": bench.MinScale, "maxScale": bench.MaxScale,
			"minDurationSeconds": bench.MinDuration, "maxDurationSeconds": bench.MaxDuration,
		},
		Actions: bench.Catalog(s.target.HasReplicas()),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if topo, err := s.target.DetectTopology(ctx); err != nil {
		resp.TargetError = err.Error()
	} else {
		resp.Topology = &topo
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var params bench.Params
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body is too large")
		return
	}
	// An empty body means "use the defaults".
	if len(body) > 0 {
		if err := json.Unmarshal(body, &params); err != nil {
			writeError(w, http.StatusBadRequest, "request body must be JSON: "+err.Error())
			return
		}
	}

	run, err := s.runner.Start(r.Context(), params)
	var paramErr *bench.ParamError
	switch {
	case errors.As(err, &paramErr):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, bench.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.log.Error("could not start a run", "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, run)
	}
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.Context(), 50)
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

type runResponse struct {
	store.Run
	Results []store.Result `json:"results"`
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	run, results, err := s.store.GetRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		s.internalError(w, "get run", err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse{Run: run, Results: results})
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	err := s.store.DeleteRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "run not found, or still running")
		return
	}
	if err != nil {
		s.internalError(w, "delete run", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// runID extracts and validates the {id} path value.
func runID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "run not found")
		return "", false
	}
	return id, true
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
