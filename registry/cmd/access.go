// Package main runs the distributed Not-So-Localhost app registry.
package main

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

type caller struct {
	user, name       string
	admin, nodeToken bool
}

func (s *Server) caller(r *http.Request) caller {
	c := caller{nodeToken: constantTimeEqual(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), s.apiToken)}
	if !constantTimeEqual(r.Header.Get("X-NSL-Proxy-Token"), s.proxyToken) {
		return c
	}
	c.user = strings.TrimSpace(r.Header.Get("X-Auth-Request-User"))
	c.name = strings.TrimSpace(r.Header.Get("X-Auth-Request-Preferred-Username"))
	for _, group := range strings.Split(r.Header.Get("X-Auth-Request-Groups"), ",") {
		if strings.TrimSpace(group) == "admin" && c.user != "" {
			c.admin = true
		}
	}
	return c
}
func (s *Server) callerInfo(w http.ResponseWriter, r *http.Request) {
	c := s.caller(r)
	if c.user == "" {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"id": c.user, "user": c.name, "admin": c.admin})
}
func (s *Server) visibleApps(r *http.Request, apps []App) []App {
	c := s.caller(r)
	return filterApps(apps, func(app App) bool { return c.nodeToken || canAccess(app, c.user, c.admin) })
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	c := s.caller(r)
	if c.user == "" {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	host := strings.ToLower(r.URL.Query().Get("host"))
	if host == "apps."+s.domain || (c.admin && s.terminalHost(host)) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.terminalHost(host) {
		state, _, err := s.repo.Load(r.Context())
		if err != nil {
			http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
			return
		}
		for _, app := range state.Apps {
			u, err := url.Parse(app.PublicURL)
			if err == nil && strings.EqualFold(u.Hostname(), host) && app.Enabled && canAccess(app, c.user, c.admin) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	http.Error(w, "forbidden", http.StatusForbidden)
}

func (s *Server) terminalHost(host string) bool {
	return strings.HasPrefix(host, "t--") && strings.HasSuffix(host, "."+s.domain) && strings.Count(host, ".") == strings.Count(s.domain, ".")+1
}

func (s *Server) transferAuthOwner(w http.ResponseWriter, r *http.Request) {
	if !s.caller(r).nodeToken || !constantTimeEqual(r.Header.Get("X-NSL-Auth-Claim"), os.Getenv("NSL_AUTH_CLAIM_TOKEN")) {
		http.Error(w, "operator credentials required", 401)
		return
	}
	var input struct {
		Expected    string `json:"expected_owner"`
		Destination string `json:"destination"`
	}
	if err := decodeJSON(r, &input); err != nil {
		http.Error(w, "invalid transfer", 400)
		return
	}
	if err := s.repo.TransferAuthOwner(r.Context(), input.Expected, input.Destination); err != nil {
		writeMutationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
