package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAccessServer(t *testing.T) *Server {
	t.Helper()
	repo := NewRepository(NewMemoryObjectStore(), "test/access")
	identity := NodeIdentity{ID: "node-1", Name: "Home", Slug: "home", CreatedAt: time.Now().UTC()}
	if err := repo.RegisterNode(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	return &Server{
		repo: repo, identity: identity, domain: "joedodge.dev",
		reconciler: NewReconciler(repo, identity.ID, "", nil, false),
		apiToken:   "api-token", proxyToken: "proxy-token", adminGroup: "admin",
	}
}

func serveAccess(server *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	server.routes(mux)
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func seedAccessApps(t *testing.T, server *Server) (adminOnly, groupApp, userApp App) {
	t.Helper()
	ctx := context.Background()
	var err error
	adminOnly, _, err = server.repo.CreateApp(ctx, AppInput{
		NodeID: "node-1",
		Name: "admin-only", TargetURL: "http://host.docker.internal:9001",
	}, server.domain)
	if err != nil {
		t.Fatal(err)
	}
	groupApp, _, err = server.repo.CreateApp(ctx, AppInput{
		NodeID: "node-1",
		Name: "family-app", TargetURL: "http://host.docker.internal:9002",
		Grant: Grant{Groups: []string{"family"}},
	}, server.domain)
	if err != nil {
		t.Fatal(err)
	}
	userApp, _, err = server.repo.CreateApp(ctx, AppInput{
		NodeID: "node-1",
		Name: "jane-app", TargetURL: "http://host.docker.internal:9003",
		Grant: Grant{Users: []string{"jane"}},
	}, server.domain)
	if err != nil {
		t.Fatal(err)
	}
	return adminOnly, groupApp, userApp
}

func decodeApps(t *testing.T, recorder *httptest.ResponseRecorder) []App {
	t.Helper()
	var apps []App
	if err := json.Unmarshal(recorder.Body.Bytes(), &apps); err != nil {
		t.Fatalf("decode apps: %v", err)
	}
	return apps
}

func TestListFilteringByCaller(t *testing.T) {
	t.Parallel()
	server := testAccessServer(t)
	seedAccessApps(t, server)

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"node token sees all", map[string]string{"Authorization": "Bearer api-token"}, 3},
		{"admin sees all", map[string]string{"X-NSL-Proxy-Token": "proxy-token", "X-Forwarded-User": "joe", "X-Forwarded-Groups": "admin"}, 3},
		{"group member sees granted", map[string]string{"X-NSL-Proxy-Token": "proxy-token", "X-Forwarded-User": "bob", "X-Forwarded-Groups": "family"}, 1},
		{"named user sees granted", map[string]string{"X-NSL-Proxy-Token": "proxy-token", "X-Forwarded-User": "jane"}, 1},
		{"stranger sees none", map[string]string{"X-NSL-Proxy-Token": "proxy-token", "X-Forwarded-User": "mallory", "X-Forwarded-Groups": "guests"}, 0},
		{"anonymous sees none", map[string]string{}, 0},
	}
	for _, tc := range cases {
		recorder := serveAccess(server, "GET", "/api/v2/apps", "", tc.headers)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.name, recorder.Code)
		}
		if got := len(decodeApps(t, recorder)); got != tc.want {
			t.Fatalf("%s: got %d apps, want %d", tc.name, got, tc.want)
		}
	}
}

func TestProxyMutationsRequireAdmin(t *testing.T) {
	t.Parallel()
	server := testAccessServer(t)
	body := `{"name":"portal-app","target_url":"http://host.docker.internal:9004"}`

	member := serveAccess(server, "POST", "/api/v2/apps", body, map[string]string{
		"X-NSL-Proxy-Token": "proxy-token", "X-Forwarded-User": "bob", "X-Forwarded-Groups": "family",
	})
	if member.Code != http.StatusForbidden {
		t.Fatalf("member create: got %d, want 403", member.Code)
	}

	admin := serveAccess(server, "POST", "/api/v2/apps", body, map[string]string{
		"X-NSL-Proxy-Token": "proxy-token", "X-Forwarded-User": "joe", "X-Forwarded-Groups": "admin",
	})
	if admin.Code != http.StatusCreated {
		t.Fatalf("admin create: got %d, want 201", admin.Code)
	}

	node := serveAccess(server, "POST", "/api/v2/apps", `{"name":"cli-app","target_url":"http://host.docker.internal:9005"}`, map[string]string{
		"Authorization": "Bearer api-token",
	})
	if node.Code != http.StatusCreated {
		t.Fatalf("node create: got %d, want 201", node.Code)
	}
}

func TestSingleAppFetchHidesUngranted(t *testing.T) {
	t.Parallel()
	server := testAccessServer(t)
	adminOnly, _, _ := seedAccessApps(t, server)

	member := serveAccess(server, "GET", "/api/v2/apps/"+adminOnly.ID, "", map[string]string{
		"X-Forwarded-User": "bob", "X-Forwarded-Groups": "family",
	})
	if member.Code != http.StatusNotFound {
		t.Fatalf("member fetch: got %d, want 404", member.Code)
	}

	admin := serveAccess(server, "GET", "/api/v2/apps/"+adminOnly.ID, "", map[string]string{
		"X-Forwarded-User": "joe", "X-Forwarded-Groups": "admin",
	})
	if admin.Code != http.StatusOK {
		t.Fatalf("admin fetch: got %d, want 200", admin.Code)
	}
}

func TestGrantRoundTripThroughUpdate(t *testing.T) {
	t.Parallel()
	server := testAccessServer(t)
	ctx := context.Background()
	created, _, err := server.repo.CreateApp(ctx, AppInput{
		NodeID: "node-1",
		Name: "flex", TargetURL: "http://host.docker.internal:9006",
	}, server.domain)
	if err != nil {
		t.Fatal(err)
	}
	admin := map[string]string{
		"X-NSL-Proxy-Token": "proxy-token", "X-Forwarded-User": "joe", "X-Forwarded-Groups": "admin",
		"Content-Type": "application/json", "If-Match": `"app:` + created.ID + `:1"`,
	}
	update := serveAccess(server, "PUT", "/api/v2/apps/"+created.ID,
		`{"name":"flex","target_url":"http://host.docker.internal:9006","grant":{"groups":[" family ", ""],"users":["jane"]}}`, admin)
	if update.Code != http.StatusOK {
		t.Fatalf("grant update: got %d", update.Code)
	}
	var updated App
	if err := json.Unmarshal(update.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Grant.Groups) != 1 || updated.Grant.Groups[0] != "family" {
		t.Fatalf("groups not normalized: %+v", updated.Grant)
	}

	member := serveAccess(server, "GET", "/api/v2/apps", "", map[string]string{
		"X-Forwarded-User": "mallory", "X-Forwarded-Groups": "family",
	})
	if got := len(decodeApps(t, member)); got != 1 {
		t.Fatalf("family member sees %d apps, want 1", got)
	}
}

func TestMeEndpoint(t *testing.T) {
	t.Parallel()
	server := testAccessServer(t)
	recorder := serveAccess(server, "GET", "/api/v2/me", "", map[string]string{
		"X-Forwarded-User": "joe", "X-Forwarded-Groups": "admin,family",
	})
	var me struct {
		User   string   `json:"user"`
		Groups []string `json:"groups"`
		Admin  bool     `json:"admin"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.User != "joe" || !me.Admin || len(me.Groups) != 2 {
		t.Fatalf("unexpected me response: %+v", me)
	}
}
