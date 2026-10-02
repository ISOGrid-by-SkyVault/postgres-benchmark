package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRequiresDatabaseURLs(t *testing.T) {
	t.Setenv("RESULTS_DATABASE_URL", "")
	t.Setenv("TARGET_DATABASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error when the database URLs are missing")
	}
	for _, name := range []string{"RESULTS_DATABASE_URL", "TARGET_DATABASE_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}

func TestLoadDefaultsAndReplicas(t *testing.T) {
	t.Setenv("RESULTS_DATABASE_URL", "postgres://results")
	t.Setenv("TARGET_DATABASE_URL", "postgres://primary")
	t.Setenv("TARGET_REPLICA_URLS", " postgres://replica-1 , ,postgres://replica-2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.TargetMaxConnections != 32 {
		t.Errorf("TargetMaxConnections = %d, want 32", cfg.TargetMaxConnections)
	}
	if got := len(cfg.TargetReplicaURLs); got != 2 {
		t.Fatalf("got %d replica URLs, want 2", got)
	}
	if cfg.TargetReplicaURLs[1] != "postgres://replica-2" {
		t.Errorf("second replica = %q", cfg.TargetReplicaURLs[1])
	}
}

func TestLoadReadsSecretFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target_url")
	if err := os.WriteFile(path, []byte("postgres://from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESULTS_DATABASE_URL", "postgres://results")
	t.Setenv("TARGET_DATABASE_URL", "postgres://ignored")
	t.Setenv("TARGET_DATABASE_URL_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TargetDatabaseURL != "postgres://from-file" {
		t.Errorf("TargetDatabaseURL = %q, want the file content", cfg.TargetDatabaseURL)
	}
}

func TestLoadRejectsBadNumbers(t *testing.T) {
	t.Setenv("RESULTS_DATABASE_URL", "postgres://results")
	t.Setenv("TARGET_DATABASE_URL", "postgres://primary")
	t.Setenv("TARGET_MAX_CONNECTIONS", "many")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a non-numeric TARGET_MAX_CONNECTIONS")
	}
}
