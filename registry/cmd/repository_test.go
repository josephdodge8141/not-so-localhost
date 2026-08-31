package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentDisjointCreatesConverge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewRepository(NewMemoryObjectStore(), "test/registry")
	repo.now = func() time.Time { return time.Unix(100, 0).UTC() }
	identity := NodeIdentity{ID: "node-1", Name: "Laptop", Slug: "laptop", CreatedAt: repo.now()}
	if err := repo.RegisterNode(ctx, identity); err != nil {
		t.Fatal(err)
	}

	const count = 16
	errors := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, _, err := repo.CreateApp(ctx, AppInput{
				NodeID: identity.ID, Name: fmt.Sprintf("app-%d", index),
				TargetURL: fmt.Sprintf("http://host.docker.internal:%d", 7000+index),
			}, "joedodge.dev")
			errors <- err
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("create app: %v", err)
		}
	}
	state, _, err := repo.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Apps) != count {
		t.Fatalf("got %d apps, want %d", len(state.Apps), count)
	}
}

func TestAppNamesAreUniquePerNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewRepository(NewMemoryObjectStore(), "test/registry")
	for _, identity := range []NodeIdentity{
		{ID: "node-a", Name: "A", Slug: "a", CreatedAt: time.Now()},
		{ID: "node-b", Name: "B", Slug: "b", CreatedAt: time.Now()},
	} {
		if err := repo.RegisterNode(ctx, identity); err != nil {
			t.Fatal(err)
		}
	}
	input := AppInput{NodeID: "node-a", Name: "API", TargetURL: "http://api:4000"}
	if _, _, err := repo.CreateApp(ctx, input, "joedodge.dev"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CreateApp(ctx, input, "joedodge.dev"); err == nil {
		t.Fatal("expected duplicate app conflict")
	}
	input.NodeID = "node-b"
	if _, _, err := repo.CreateApp(ctx, input, "joedodge.dev"); err != nil {
		t.Fatalf("same app name on another node: %v", err)
	}
}

func TestStaleAppGenerationDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewRepository(NewMemoryObjectStore(), "test/registry")
	if err := repo.RegisterNode(ctx, NodeIdentity{ID: "node", Name: "Node", Slug: "node", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	created, _, err := repo.CreateApp(ctx, AppInput{NodeID: "node", Name: "API", TargetURL: "http://api:4000"}, "joedodge.dev")
	if err != nil {
		t.Fatal(err)
	}
	input := AppInput{NodeID: "node", Name: "API", Description: "new", TargetURL: "http://api:4000"}
	if _, _, err := repo.UpdateApp(ctx, created.ID, created.Generation, input, "joedodge.dev"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.UpdateApp(ctx, created.ID, created.Generation, input, "joedodge.dev"); err == nil {
		t.Fatal("expected stale generation conflict")
	}
}

func TestAuthOwnerCanOnlyBeClaimedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewRepository(NewMemoryObjectStore(), "test/registry")
	for _, identity := range []NodeIdentity{
		{ID: "node-a", Name: "A", Slug: "a", CreatedAt: time.Now()},
		{ID: "node-b", Name: "B", Slug: "b", CreatedAt: time.Now()},
	} {
		if err := repo.RegisterNode(ctx, identity); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ClaimAuthOwner(ctx, "node-a"); err != nil {
		t.Fatal(err)
	}
	if err := repo.ClaimAuthOwner(ctx, "node-b"); err == nil {
		t.Fatal("expected second auth owner claim to fail")
	}
}

func TestMissingCurrentPointerFailsClosedAfterInitialization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryObjectStore()
	repo := NewRepository(store, "test/registry")
	if err := repo.RegisterNode(ctx, NodeIdentity{ID: "node", Name: "Node", Slug: "node", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	delete(store.objects, repo.currentKey())
	store.mu.Unlock()
	if _, _, err := repo.Load(ctx); err == nil {
		t.Fatal("expected missing pointer to fail closed")
	}
}

func TestRouteCannotClaimAnotherHostname(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewRepository(NewMemoryObjectStore(), "test/registry")
	if err := repo.RegisterNode(ctx, NodeIdentity{ID: "node", Name: "Node", Slug: "node", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, _, err := repo.CreateApp(ctx, AppInput{
		NodeID: "node", Name: "api", TargetURL: "http://api:4000",
		Routes: []RouteInput{{Rule: "Host(`apps.joedodge.dev`)", Auth: AuthUpstream}},
	}, "joedodge.dev")
	if err == nil {
		t.Fatal("expected reserved hostname route to be rejected")
	}
}

func TestAppCannotClaimTerminalHostname(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewRepository(NewMemoryObjectStore(), "test/registry")
	if err := repo.RegisterNode(ctx, NodeIdentity{ID: "node", Name: "Node", Slug: "node", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, _, err := repo.CreateApp(ctx, AppInput{
		NodeID: "node", Name: "t", TargetURL: "http://terminal:7681",
	}, "joedodge.dev")
	if err == nil {
		t.Fatal("expected terminal app name to be rejected")
	}
}

func TestAppETagIncludesIdentity(t *testing.T) {
	t.Parallel()
	id, generation, err := parseAppETag(`"app:app-one:7"`)
	if err != nil || id != "app-one" || generation != 7 {
		t.Fatalf("parsed %q %d: %v", id, generation, err)
	}
}

func TestLiteLLMRoutesShareOneService(t *testing.T) {
	t.Parallel()
	apps := []App{{
		ID: "11111111-1111-4111-8111-111111111111", TargetURL: "http://litellm:4000", Enabled: true,
		Routes: []Route{
			{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Rule: "Host(`litellm--mac.joedodge.dev`) && !PathPrefix(`/v1`)", Priority: 100, Auth: AuthBrowser},
			{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Rule: "Host(`litellm--mac.joedodge.dev`) && PathPrefix(`/v1`)", Priority: 110, Auth: AuthUpstream},
		},
	}}
	rendered := string(renderTraefik(apps))
	if count := strings.Count(rendered, "loadBalancer:"); count != 1 {
		t.Fatalf("got %d services, want 1\n%s", count, rendered)
	}
	if count := strings.Count(rendered, "middlewares:"); count != 1 {
		t.Fatalf("got %d auth middlewares, want 1\n%s", count, rendered)
	}
}

func TestEmptyReconciliationRemovesManagedRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewRepository(NewMemoryObjectStore(), "test/registry")
	directory := t.TempDir()
	path := filepath.Join(directory, "managed.yml")
	if err := os.WriteFile(path, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	reconciler := NewReconciler(repo, "node", directory, nil, false)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("managed route file still exists: %v", err)
	}
}

func TestReconcilersOnlyRenderOwnedApps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewRepository(NewMemoryObjectStore(), "test/registry")
	for _, identity := range []NodeIdentity{
		{ID: "node-a", Name: "A", Slug: "a", CreatedAt: time.Now()},
		{ID: "node-b", Name: "B", Slug: "b", CreatedAt: time.Now()},
	} {
		if err := repo.RegisterNode(ctx, identity); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []AppInput{
		{NodeID: "node-a", Name: "alpha", TargetURL: "http://alpha:3000"},
		{NodeID: "node-b", Name: "beta", TargetURL: "http://beta:4000"},
	} {
		if _, _, err := repo.CreateApp(ctx, input, "joedodge.dev"); err != nil {
			t.Fatal(err)
		}
	}
	for nodeID, expected := range map[string]string{"node-a": "http://alpha:3000", "node-b": "http://beta:4000"} {
		directory := filepath.Join(t.TempDir(), nodeID)
		reconciler := NewReconciler(repo, nodeID, directory, nil, false)
		if err := reconciler.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(directory, "managed.yml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), expected) {
			t.Fatalf("%s routes do not contain %s: %s", nodeID, expected, body)
		}
		other := "http://alpha:3000"
		if nodeID == "node-a" {
			other = "http://beta:4000"
		}
		if strings.Contains(string(body), other) {
			t.Fatalf("%s rendered another node's app: %s", nodeID, body)
		}
	}
}
