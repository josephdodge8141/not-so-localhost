package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthorizationIntegration(t *testing.T) {
	repo := NewRepository(NewMemoryObjectStore(), "test/access")
	identity := NodeIdentity{ID: "node-1", Name: "Home", Slug: "home", CreatedAt: time.Now().UTC()}
	if err := repo.RegisterNode(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	s := &Server{repo: repo, identity: identity, domain: "example.test", apiToken: "node-token", proxyToken: "proxy-token", reconciler: NewReconciler(repo, identity.ID, "", nil, false)}
	app, _, err := repo.CreateApp(context.Background(), AppInput{NodeID: identity.ID, Name: "assigned", TargetURL: "http://localhost:9000", Grant: &Grant{Users: []string{"subject-member"}}}, s.domain)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	call := func(method, path, user, groups, token, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		req.Header.Set("X-NSL-Proxy-Token", token)
		req.Header.Set("X-Auth-Request-User", user)
		req.Header.Set("X-Auth-Request-Groups", groups)
		req.Header.Set("If-Match", appETag(app))
		if token == "node-token" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	for _, tc := range []struct {
		name, path, user, groups, token string
		status                          int
	}{
		{"missing identity", "/api/v2/me", "", "", "proxy-token", 401},
		{"forged admin", "/api/v2/me", "attacker", "admin", "", 401},
		{"admin identity", "/api/v2/me", "subject-admin", "admin", "proxy-token", 200},
		{"member app", "/internal/authorize?host=assigned--home.example.test", "subject-member", "", "proxy-token", 204},
		{"unassigned app", "/internal/authorize?host=assigned--home.example.test", "other", "", "proxy-token", 403},
		{"username is not subject", "/internal/authorize?host=assigned--home.example.test", "member", "", "proxy-token", 403},
		{"admin app", "/internal/authorize?host=assigned--home.example.test", "subject-admin", "admin", "proxy-token", 204},
		{"admin terminal", "/internal/authorize?host=t--home.example.test", "subject-admin", "admin", "proxy-token", 204},
		{"member terminal", "/internal/authorize?host=t--home.example.test", "subject-member", "", "proxy-token", 403},
		{"forged terminal", "/internal/authorize?host=t--home.example.test", "attacker", "admin", "", 401},
		{"unknown host", "/internal/authorize?host=unknown.example.test", "subject-admin", "admin", "proxy-token", 403},
		{"hidden v2", "/api/v2/apps/" + app.ID, "other", "", "proxy-token", 404},
		{"hidden v1", "/api/v1/apps/" + app.ID, "other", "", "proxy-token", 404},
		{"directory denied", "/api/v2/users", "subject-member", "", "proxy-token", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if res := call("GET", tc.path, tc.user, tc.groups, tc.token, ""); res.StatusCode != tc.status {
				t.Fatalf("got %d want %d", res.StatusCode, tc.status)
			}
		})
	}
	for _, prefix := range []string{"/api/v2/apps", "/api/v1/apps", "/api/apps"} {
		for _, tc := range []struct {
			user, groups, token string
			want                int
		}{{"subject-member", "", "proxy-token", 1}, {"other", "", "proxy-token", 0}, {"attacker", "admin", "", 0}, {"subject-admin", "admin", "proxy-token", 1}, {"", "", "node-token", 1}} {
			res := call("GET", prefix, tc.user, tc.groups, tc.token, "")
			var apps []json.RawMessage
			if err := json.NewDecoder(res.Body).Decode(&apps); err != nil || len(apps) != tc.want {
				t.Fatalf("list %s: %v count %d want %d", prefix, err, len(apps), tc.want)
			}
		}
	}
	body := `{"name":"assigned","target_url":"http://localhost:9000"}`
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		path := "/api/v2/apps"
		if method != "POST" {
			path += "/" + app.ID
		}
		if res := call(method, path, "subject-member", "", "proxy-token", body); res.StatusCode != 403 {
			t.Fatalf("member %s: %d", method, res.StatusCode)
		}
	}
	for _, prefix := range []string{"/api/v2/apps/", "/api/v1/apps/"} {
		res := call("PUT", prefix+app.ID, "", "", "node-token", body)
		if res.StatusCode != 200 {
			t.Fatalf("CLI update: %d", res.StatusCode)
		}
		state, _, _ := repo.Load(context.Background())
		app = state.Apps[0]
		if len(app.Grant.Users) != 1 || app.Grant.Users[0] != "subject-member" {
			t.Fatal("CLI erased assignments")
		}
	}
	app, _, err = repo.UpdateApp(context.Background(), app.ID, app.Generation, AppInput{NodeID: identity.ID, Name: app.Name, TargetURL: app.TargetURL, Grant: &Grant{}}, s.domain)
	if err != nil {
		t.Fatal(err)
	}
	if res := call("GET", "/internal/authorize?host=assigned--home.example.test", "subject-member", "", "proxy-token", ""); res.StatusCode != 403 {
		t.Fatal("revocation failed")
	}
	s.repo = NewRepository(unavailableStore{}, "test")
	if res := call("GET", "/internal/authorize?host=assigned--home.example.test", "subject-member", "", "proxy-token", ""); res.StatusCode != 503 {
		t.Fatalf("outage must fail closed: %d", res.StatusCode)
	}

}

type unavailableStore struct{ ObjectStore }

func (unavailableStore) Get(context.Context, string) (StoredObject, error) {
	return StoredObject{}, errors.New("offline")
}

func TestAuthOwnerTransfer(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryObjectStore()
	repo := NewRepository(store, "transfer")
	for _, id := range []string{"source", "destination"} {
		if err := repo.RegisterNode(ctx, NodeIdentity{ID: id, Name: id, Slug: id, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ClaimAuthOwner(ctx, "source"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NSL_AUTH_CLAIM_TOKEN", "operator-secret")
	s := &Server{repo: repo, apiToken: "node-secret"}
	mux := http.NewServeMux()
	s.routes(mux)
	for _, tc := range []struct {
		expected, destination, claim string
		want                         int
	}{
		{"source", "destination", "", 401}, {"stale", "destination", "operator-secret", 409},
		{"source", "missing", "operator-secret", 400}, {"source", "destination", "operator-secret", 204},
		{"source", "destination", "operator-secret", 204}, {"destination", "source", "operator-secret", 204},
	} {
		r := httptest.NewRequest("POST", "/internal/auth-owner", strings.NewReader(`{"expected_owner":"`+tc.expected+`","destination":"`+tc.destination+`"}`))
		r.Header.Set("Authorization", "Bearer node-secret")
		r.Header.Set("X-NSL-Auth-Claim", tc.claim)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("transfer: got %d want %d", w.Code, tc.want)
		}
	}
	state, _, err := NewRepository(store, "transfer").Load(ctx)
	if err != nil || state.AuthOwnerNodeID != "source" {
		t.Fatalf("rollback: %v %v", state.AuthOwnerNodeID, err)
	}
}
