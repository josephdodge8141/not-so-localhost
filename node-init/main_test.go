package main

import (
	"path/filepath"
	"testing"
)

func TestIdentityIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	first, err := loadOrCreateIdentity(path, "Laptop Three")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateIdentity(path, "Laptop Three")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("identity changed: %#v != %#v", first, second)
	}
	if first.Slug != "laptop-three" {
		t.Fatalf("slug = %q", first.Slug)
	}
}

func TestIdentityRejectsRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	if _, err := loadOrCreateIdentity(path, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateIdentity(path, "desktop"); err == nil {
		t.Fatal("expected rename failure")
	}
}
