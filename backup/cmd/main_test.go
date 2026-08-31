package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTargetsUsesEnvironmentSecret(t *testing.T) {
	t.Setenv("TEST_DB_PASSWORD", "secret")
	directory := t.TempDir()
	file := filepath.Join(directory, "targets.json")
	configured := []TargetConfig{{
		ID: "litellm", Label: "LiteLLM", Type: "postgres", Name: "litellm",
		User: "litellm", PasswordEnv: "TEST_DB_PASSWORD", Host: "database", Port: 5432,
	}}
	body, err := json.Marshal(configured)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	targets, err := loadTargets(Config{TargetsFile: file})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Password != "secret" {
		t.Fatalf("targets = %#v", targets)
	}
}

func TestLoadTargetsRejectsDuplicateIDs(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "targets.json")
	body := []byte(`[
      {"id":"db","type":"postgres","name":"one","user":"one","host":"db","password_env":"DB_ONE"},
      {"id":"db","type":"postgres","name":"two","user":"two","host":"db","password_env":"DB_TWO"}
    ]`)
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_ONE", "postgres://one")
	t.Setenv("DB_TWO", "postgres://two")
	if _, err := loadTargets(Config{TargetsFile: file}); err == nil {
		t.Fatal("expected duplicate target error")
	}
}

func TestParseBackupTime(t *testing.T) {
	t.Parallel()
	parsed := parseBackupTime("backups/v2/node/db/20260828T120102.123456789Z.sql.gz")
	if parsed == nil || parsed.UTC().Year() != 2026 {
		t.Fatalf("parsed = %v", parsed)
	}
}
