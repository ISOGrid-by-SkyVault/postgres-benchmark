// Package config reads the runtime configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything the server needs to start. Load fails when a required
// value is missing or malformed, so a misconfigured deployment exits at once
// instead of starting half-working.
type Config struct {
	// Port the HTTP server listens on.
	Port string
	// ResultsDatabaseURL is the PostgreSQL database that stores benchmark runs.
	ResultsDatabaseURL string
	// TargetDatabaseURL is the read-write endpoint of the database under test.
	TargetDatabaseURL string
	// TargetReplicaURLs are optional read-only endpoints. When set, read
	// benchmarks are spread over them and replication lag is measured.
	TargetReplicaURLs []string
	// TargetLabel names the database under test in the UI and in saved runs.
	TargetLabel string
	// TargetMaxConnections caps each connection pool to the target, and
	// therefore the concurrency a run may ask for.
	TargetMaxConnections int32
	// HealthInterval is how often the results database is checked.
	HealthInterval time.Duration
	// HealthFailureThreshold is how many consecutive failed checks are
	// tolerated before the process exits with a non-zero status.
	HealthFailureThreshold int
}

// Load builds the configuration from environment variables.
func Load() (Config, error) {
	cfg := Config{
		Port:        envOr("PORT", "8080"),
		TargetLabel: envOr("TARGET_LABEL", "PostgreSQL"),
	}

	var err error
	if cfg.ResultsDatabaseURL, err = secret("RESULTS_DATABASE_URL"); err != nil {
		return cfg, err
	}
	if cfg.TargetDatabaseURL, err = secret("TARGET_DATABASE_URL"); err != nil {
		return cfg, err
	}
	replicas, err := secret("TARGET_REPLICA_URLS")
	if err != nil {
		return cfg, err
	}
	cfg.TargetReplicaURLs = splitList(replicas)

	var missing []string
	if cfg.ResultsDatabaseURL == "" {
		missing = append(missing, "RESULTS_DATABASE_URL")
	}
	if cfg.TargetDatabaseURL == "" {
		missing = append(missing, "TARGET_DATABASE_URL")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	maxConns, err := intEnv("TARGET_MAX_CONNECTIONS", 32)
	if err != nil {
		return cfg, err
	}
	if maxConns < 1 || maxConns > 512 {
		return cfg, errors.New("TARGET_MAX_CONNECTIONS must be between 1 and 512")
	}
	cfg.TargetMaxConnections = int32(maxConns)

	interval, err := intEnv("HEALTH_INTERVAL_SECONDS", 10)
	if err != nil {
		return cfg, err
	}
	if interval < 1 {
		return cfg, errors.New("HEALTH_INTERVAL_SECONDS must be at least 1")
	}
	cfg.HealthInterval = time.Duration(interval) * time.Second

	if cfg.HealthFailureThreshold, err = intEnv("HEALTH_FAILURE_THRESHOLD", 6); err != nil {
		return cfg, err
	}
	if cfg.HealthFailureThreshold < 1 {
		return cfg, errors.New("HEALTH_FAILURE_THRESHOLD must be at least 1")
	}

	return cfg, nil
}

// secret reads NAME, or the file named by NAME_FILE. The file form is how
// Docker Swarm and Kubernetes hand secrets to a container.
func secret(name string) (string, error) {
	if path := os.Getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func intEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, raw)
	}
	return value, nil
}

// splitList parses a comma-separated list, ignoring empty entries.
func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
