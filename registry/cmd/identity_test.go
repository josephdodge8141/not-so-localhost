package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityPersistsAcrossStartup(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "node.json")
	first, err := LoadOrCreateIdentity(path, "Laptop Three")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateIdentity(path, "Laptop Three")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("identity changed: %#v != %#v", first, second)
	}
	if second.Slug != "laptop-three" {
		t.Fatalf("slug = %q", second.Slug)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestIdentityRejectsNodeRename(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "node.json")
	if _, err := LoadOrCreateIdentity(path, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(path, "desktop"); err == nil {
		t.Fatal("expected persisted name mismatch")
	}
}
