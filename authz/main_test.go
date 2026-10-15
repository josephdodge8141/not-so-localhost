package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testChecker(registry *httptest.Server) *checker {
	return &checker{
		registryURL: registry.URL,
		domain:      "joedodge.dev",
		adminGroup:  "admin",
		client:      registry.Client(),
	}
}

func stubRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	apps := []app{
		{PublicURL: "https://landing--home.joedodge.dev", Enabled: true},
		{PublicURL: "https://family--home.joedodge.dev", Enabled: true, Grant: appGrant{Groups: []string{"family"}}},
		{PublicURL: "https://jane--home.joedodge.dev", Enabled: true, Grant: appGrant{Users: []string{"jane"}}},
		{PublicURL: "https://off--home.joedodge.dev", Enabled: false, Grant: appGrant{Groups: []string{"family"}}},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apps)
	}))
}

func TestAuthorizeMatrix(t *testing.T) {
	t.Parallel()
	check := testChecker(stubRegistry(t))
	mux := http.NewServeMux()
	mux.HandleFunc("/", check.authorize)

	cases := []struct {
		name   string
		host   string
		user   string
		groups string
		want   int
	}{
		{"portal open to members", "apps.joedodge.dev", "bob", "family", http.StatusNoContent},
		{"portal needs login", "apps.joedodge.dev", "", "", http.StatusForbidden},
		{"admin ttyd", "t--home.joedodge.dev", "joe", "admin", http.StatusNoContent},
		{"member ttyd denied", "t--home.joedodge.dev", "bob", "family", http.StatusForbidden},
		{"admin sees admin-only app", "landing--home.joedodge.dev", "joe", "admin", http.StatusNoContent},
		{"member denied admin-only app", "landing--home.joedodge.dev", "bob", "family", http.StatusForbidden},
		{"group grant allows", "family--home.joedodge.dev", "bob", "family", http.StatusNoContent},
		{"user grant allows", "jane--home.joedodge.dev", "jane", "", http.StatusNoContent},
		{"wrong user denied", "jane--home.joedodge.dev", "bob", "family", http.StatusForbidden},
		{"disabled app denied", "off--home.joedodge.dev", "bob", "family", http.StatusForbidden},
		{"unknown host denied", "nope--home.joedodge.dev", "joe", "admin", http.StatusForbidden},
		{"anonymous denied", "family--home.joedodge.dev", "", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil)
		if tc.user != "" {
			request.Header.Set("X-Forwarded-User", tc.user)
		}
		if tc.groups != "" {
			request.Header.Set("X-Forwarded-Groups", tc.groups)
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.name, recorder.Code, tc.want)
		}
	}
}

func TestRegistryOutageFailsClosed(t *testing.T) {
	t.Parallel()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	check := testChecker(dead)
	request := httptest.NewRequest(http.MethodGet, "http://family--home.joedodge.dev/", nil)
	request.Header.Set("X-Forwarded-User", "bob")
	recorder := httptest.NewRecorder()
	check.authorize(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", recorder.Code)
	}
}
