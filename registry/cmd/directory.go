// Package main runs the distributed Not-So-Localhost app registry.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type directoryUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Enabled  bool   `json:"enabled"`
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	if !s.caller(r).admin {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	users, err := searchUsers(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, "user directory unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, users)
}
func searchUsers(ctx context.Context, query string) ([]directoryUser, error) {
	base := strings.TrimRight(os.Getenv("KEYCLOAK_URL"), "/")
	secret := os.Getenv("KEYCLOAK_DIRECTORY_SECRET")
	if base == "" || secret == "" {
		return nil, fmt.Errorf("directory not configured")
	}
	client := &http.Client{Timeout: 8 * time.Second}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {"registry"}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/realms/local/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("directory token status %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&token); err != nil {
		return nil, err
	}
	params := url.Values{"search": {query}, "max": {"50"}, "enabled": {"true"}, "briefRepresentation": {"true"}}
	req, err = http.NewRequestWithContext(ctx, "GET", base+"/admin/realms/local/users?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	res, err = client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("directory status %d", res.StatusCode)
	}
	users := []directoryUser{}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&users)
	return users, err
}
