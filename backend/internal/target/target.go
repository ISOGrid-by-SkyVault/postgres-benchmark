// Package target manages the connections to the database under test and
// detects whether it is a single node or a multi-node cluster.
package target

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Topology modes.
const (
	ModeSingle    = "single"
	ModeMultiNode = "multi-node"
)

// Target is the database under test: one read-write endpoint and, optionally,
// read-only replica endpoints.
type Target struct {
	Label    string
	Primary  *pgxpool.Pool
	Replicas []*pgxpool.Pool
	maxConns int32
}

// Topology describes the shape of the target as seen at one moment.
type Topology struct {
	// Mode is "single" or "multi-node".
	Mode string `json:"mode"`
	// NodeCount is the primary plus every standby known to it or configured.
	NodeCount     int    `json:"nodeCount"`
	ServerVersion string `json:"serverVersion"`
	// StreamingReplicas is the number of standbys the primary reports in
	// pg_stat_replication. A managed cluster behind a single address shows up
	// here even though the application only knows one endpoint.
	StreamingReplicas int `json:"streamingReplicas"`
	// ReadEndpoints is the number of replica URLs the application was given.
	ReadEndpoints int `json:"readEndpoints"`
}

// Connect opens the pools and verifies every endpoint answers.
func Connect(ctx context.Context, label, primaryURL string, replicaURLs []string, maxConns int32) (*Target, error) {
	t := &Target{Label: label, maxConns: maxConns}

	primary, err := open(ctx, primaryURL, maxConns)
	if err != nil {
		return nil, fmt.Errorf("target database: %w", err)
	}
	t.Primary = primary

	for i, url := range replicaURLs {
		replica, err := open(ctx, url, maxConns)
		if err != nil {
			t.Close()
			return nil, fmt.Errorf("target replica %d: %w", i+1, err)
		}
		t.Replicas = append(t.Replicas, replica)
	}
	return t, nil
}

func open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Close releases every pool.
func (t *Target) Close() {
	if t.Primary != nil {
		t.Primary.Close()
	}
	for _, replica := range t.Replicas {
		replica.Close()
	}
}

// MaxConcurrency is the highest worker count a run may use.
func (t *Target) MaxConcurrency() int { return int(t.maxConns) }

// HasReplicas reports whether read-only endpoints were configured.
func (t *Target) HasReplicas() bool { return len(t.Replicas) > 0 }

// Reader returns the pool a read worker should use: replicas in round-robin
// when there are any, the primary otherwise.
func (t *Target) Reader(worker int) *pgxpool.Pool {
	if len(t.Replicas) == 0 {
		return t.Primary
	}
	return t.Replicas[worker%len(t.Replicas)]
}

// ReaderNode names where reads go.
func (t *Target) ReaderNode() string {
	if t.HasReplicas() {
		return "replica"
	}
	return "primary"
}

// Ping checks the read-write endpoint.
func (t *Target) Ping(ctx context.Context) error { return t.Primary.Ping(ctx) }

// DetectTopology asks the primary for its version and standbys.
func (t *Target) DetectTopology(ctx context.Context) (Topology, error) {
	topo := Topology{ReadEndpoints: len(t.Replicas)}

	if err := t.Primary.QueryRow(ctx, `SHOW server_version`).Scan(&topo.ServerVersion); err != nil {
		return topo, fmt.Errorf("target database: %w", err)
	}

	// Without the pg_monitor role a user still sees one row per standby, with
	// the details hidden, so the count is reliable. If the view cannot be read
	// at all, fall back to the configured endpoints.
	if err := t.Primary.QueryRow(ctx, `SELECT count(*) FROM pg_stat_replication`).Scan(&topo.StreamingReplicas); err != nil {
		topo.StreamingReplicas = 0
	}

	standbys := max(topo.StreamingReplicas, topo.ReadEndpoints)
	topo.NodeCount = 1 + standbys
	topo.Mode = ModeSingle
	if standbys > 0 {
		topo.Mode = ModeMultiNode
	}
	return topo, nil
}
