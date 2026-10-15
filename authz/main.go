// Package main enforces per-app authorization for browser routes.
//
// Traefik chains this service after the oauth2-proxy login check, so every
// request arriving here is already authenticated. Identity arrives in the
// X-Forwarded-* headers attached by that check. This service must only be
// reachable through those chains: direct callers could spoof the headers.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type appGrant struct {
	Groups []string `json:"groups"`
	Users  []string `json:"users"`
}

type app struct {
	PublicURL string   `json:"public_url"`
	Enabled   bool     `json:"enabled"`
	Grant     appGrant `json:"grant"`
}

type checker struct {
	registryURL string
	domain      string
	adminGroup  string
	client      *http.Client
}

func main() {
	check := &checker{
		registryURL: getEnv("REGISTRY_URL", "http://registry:7272"),
		domain:      strings.ToLower(strings.TrimSpace(requiredEnv("DOMAIN"))),
		adminGroup:  getEnv("NSL_ADMIN_GROUP", "admin"),
		client:      &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", check.authorize)
	server := &http.Server{
		Addr:              getEnv("AUTHZ_ADDR", ":4181"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("authz on %s (domain %s)", server.Addr, check.domain)
	log.Fatal(server.ListenAndServe())
}

func (c *checker) authorize(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(stripPort(r.Host))
	user := forwardedUser(r)
	groups := forwardedGroups(r)
	if user == "" {
		deny(w, "login required")
		return
	}
	if host == "apps."+c.domain {
		allow(w)
		return
	}
	if isTerminalHost(host, c.domain) {
		if isAdmin(groups, c.adminGroup) {
			allow(w)
			return
		}
		deny(w, "admin only")
		return
	}
	apps, err := c.loadApps(r.Context())
	if err != nil {
		log.Printf("load apps: %v", err)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, app := range apps {
		if appHost(app.PublicURL) != host || !app.Enabled {
			continue
		}
		if granted(app.Grant, user, groups, isAdmin(groups, c.adminGroup)) {
			allow(w)
			return
		}
		deny(w, "forbidden")
		return
	}
	deny(w, "unknown app")
}

// granted mirrors canAccess in registry/cmd/model.go. The two services share
// no package, so the rule lives in both places; keep them in sync.
func granted(grant appGrant, user string, groups []string, admin bool) bool {
	if admin {
		return true
	}
	for _, allowed := range grant.Users {
		if allowed == user {
			return true
		}
	}
	for _, member := range groups {
		for _, allowed := range grant.Groups {
			if allowed == member {
				return true
			}
		}
	}
	return false
}

func (c *checker) loadApps(ctx context.Context) ([]app, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.registryURL+"/api/v2/apps", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var apps []app
	if err := json.Unmarshal(body, &apps); err != nil {
		return nil, err
	}
	return apps, nil
}

func forwardedUser(r *http.Request) string {
	if user := strings.TrimSpace(r.Header.Get("X-Forwarded-Preferred-Username")); user != "" {
		return user
	}
	return strings.TrimSpace(r.Header.Get("X-Forwarded-User"))
}

func forwardedGroups(r *http.Request) []string {
	var groups []string
	for _, group := range strings.Split(r.Header.Get("X-Forwarded-Groups"), ",") {
		if trimmed := strings.TrimSpace(group); trimmed != "" {
			groups = append(groups, trimmed)
		}
	}
	return groups
}

func isAdmin(groups []string, adminGroup string) bool {
	for _, group := range groups {
		if group == adminGroup {
			return true
		}
	}
	return false
}

func isTerminalHost(host, domain string) bool {
	return strings.HasPrefix(host, "t--") && strings.HasSuffix(host, "."+domain)
}

func stripPort(hostport string) string {
	if host, _, ok := strings.Cut(hostport, ":"); ok {
		return host
	}
	return hostport
}

func appHost(publicURL string) string {
	parsed, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func allow(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func deny(w http.ResponseWriter, reason string) {
	http.Error(w, reason, http.StatusForbidden)
}

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

func getEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
