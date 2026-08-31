// Package main runs the distributed Not-So-Localhost app registry.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed index.html
var frontend embed.FS

var version = "dev"

type Server struct {
	repo       *Repository
	identity   NodeIdentity
	domain     string
	reconciler *Reconciler
	broker     *BrokerClient
	apiToken   string
	proxyToken string
}

type mutationSourceKey struct{}

func main() {
	ctx := context.Background()
	domain := requiredEnv("DOMAIN")
	nodeName := requiredEnv("NODE_NAME")
	identityPath := getEnv("NODE_IDENTITY_FILE", "/var/lib/nsl/node.json")
	identity, err := LoadOrCreateIdentity(identityPath, nodeName)
	if err != nil {
		log.Fatal(err)
	}

	store, err := NewS3ObjectStore(ctx, S3Config{
		Bucket: getEnv("REGISTRY_S3_BUCKET", ""),
		Region: getEnv("AWS_REGION", "us-east-1"),
	})
	if err != nil {
		log.Fatalf("configure registry store: %v", err)
	}
	repo := NewRepository(store, getEnv("REGISTRY_S3_PREFIX", "nsl/registry/v1"))
	if err := repo.RegisterNode(ctx, identity); err != nil {
		log.Fatalf("register node: %v", err)
	}
	if strings.EqualFold(getEnv("NSL_AUTH_OWNER", "false"), "true") {
		if err := repo.ClaimAuthOwner(ctx, identity.ID); err != nil {
			log.Fatalf("claim auth ownership: %v", err)
		}
	}

	broker := NewBrokerClient(
		getEnv("ENROLLMENT_BROKER_URL", ""),
		getEnv("NODE_CREDENTIAL_FILE", "/var/lib/nsl/node-credential"),
		identity.Slug,
	)
	authOwner := strings.EqualFold(getEnv("NSL_AUTH_OWNER", "false"), "true")
	reconciler := NewReconciler(repo, identity.ID, getEnv("TRAEFIK_ROUTES_DIR", ""), broker, authOwner)
	if err := reconciler.Reconcile(ctx); err != nil {
		log.Printf("initial route reconciliation: %v", err)
	}
	go reconciler.Run(ctx, 10*time.Second)

	server := &Server{
		repo: repo, identity: identity, domain: domain, reconciler: reconciler, broker: broker,
		apiToken: requiredEnv("REGISTRY_API_TOKEN"), proxyToken: requiredEnv("REGISTRY_PROXY_TOKEN"),
	}
	mux := http.NewServeMux()
	server.routes(mux)

	httpServer := &http.Server{
		Addr:              ":7272",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("registry %s on :7272 (node %s, %s)", version, identity.Name, identity.ID)
	log.Fatal(httpServer.ListenAndServe())
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /{$}", serveFrontend)
	mux.HandleFunc("GET /api/config", s.apiConfig)

	mux.HandleFunc("GET /api/v2/version", apiVersion)
	mux.HandleFunc("GET /api/v2/node", s.localNode)
	mux.HandleFunc("GET /api/v2/auth-owner", s.authOwner)
	mux.HandleFunc("GET /api/v2/auth-ready", s.authReady)
	mux.HandleFunc("GET /api/v2/nodes", s.listNodes)
	mux.HandleFunc("GET /api/v2/apps", s.listApps)
	mux.HandleFunc("POST /api/v2/apps", s.requireMutationAuth(s.createApp))
	mux.HandleFunc("GET /api/v2/apps/{id}", s.getApp)
	mux.HandleFunc("PUT /api/v2/apps/{id}", s.requireMutationAuth(s.updateApp))
	mux.HandleFunc("DELETE /api/v2/apps/{id}", s.requireMutationAuth(s.deleteApp))

	// v1 remains a direct-service adapter for older nsl clients.
	mux.HandleFunc("GET /api/v1/version", apiVersion)
	mux.HandleFunc("GET /api/v1/apps", s.listLegacyApps)
	mux.HandleFunc("POST /api/v1/apps", s.requireMutationAuth(s.createLegacyApp))
	mux.HandleFunc("GET /api/v1/apps/{id}", s.getLegacyApp)
	mux.HandleFunc("PUT /api/v1/apps/{id}", s.requireMutationAuth(s.updateLegacyApp))
	mux.HandleFunc("DELETE /api/v1/apps/{id}", s.requireMutationAuth(s.deleteLegacyApp))
	mux.HandleFunc("GET /api/apps", s.listLegacyApps)
	mux.HandleFunc("POST /api/apps", s.requireMutationAuth(s.createLegacyApp))
	mux.HandleFunc("GET /api/apps/{id}", s.getLegacyApp)
	mux.HandleFunc("PUT /api/apps/{id}", s.requireMutationAuth(s.updateLegacyApp))
	mux.HandleFunc("DELETE /api/apps/{id}", s.requireMutationAuth(s.deleteLegacyApp))
}

func apiVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version})
}

func (s *Server) apiConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"domain":    s.domain,
		"node_id":   s.identity.ID,
		"node_name": s.identity.Name,
		"node_slug": s.identity.Slug,
	})
}

func (s *Server) localNode(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.identity)
}

func (s *Server) authOwner(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if state.AuthOwnerNodeID != s.identity.ID {
		http.Error(w, "this node does not own auth", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authReady(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if state.AuthOwnerNodeID != s.identity.ID {
		http.Error(w, "this node does not own auth", http.StatusConflict)
		return
	}
	if s.broker == nil {
		http.Error(w, "enrollment broker is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.broker.EnsureAuth(r.Context()); err != nil {
		http.Error(w, "auth hostname is not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireMutationAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proxyTrusted := constantTimeEqual(r.Header.Get("X-NSL-Proxy-Token"), s.proxyToken)
		apiTrusted := constantTimeEqual(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), s.apiToken)
		if !proxyTrusted && !apiTrusted {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		source := "node"
		if proxyTrusted {
			source = "proxy"
		}
		next(w, r.WithContext(context.WithValue(r.Context(), mutationSourceKey{}, source)))
	}
}

func isNodeMutation(r *http.Request) bool {
	return r.Context().Value(mutationSourceKey{}) == "node"
}

func constantTimeEqual(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeRevision(w, state.Revision)
	writeJSON(w, http.StatusOK, state.Nodes)
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	apps := state.Apps
	if nodeID := r.URL.Query().Get("node_id"); nodeID != "" {
		apps = filterApps(apps, func(app App) bool { return app.NodeID == nodeID })
	}
	writeRevision(w, state.Revision)
	writeJSON(w, http.StatusOK, apps)
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	app, ok := findApp(state.Apps, r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("ETag", appETag(app))
	writeRevision(w, state.Revision)
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var input AppInput
	if err := decodeJSON(r, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if input.NodeID == "" {
		input.NodeID = s.identity.ID
	}
	if isNodeMutation(r) && input.NodeID != s.identity.ID {
		http.Error(w, "node credentials may only register local apps", http.StatusForbidden)
		return
	}
	app, revision, err := s.repo.CreateApp(r.Context(), input, s.domain)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	s.reconciler.Trigger()
	w.Header().Set("ETag", appETag(app))
	writeRevision(w, revision)
	writeJSON(w, http.StatusCreated, app)
}

func (s *Server) updateApp(w http.ResponseWriter, r *http.Request) {
	var input AppInput
	if err := decodeJSON(r, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	etagID, generation, err := parseAppETag(r.Header.Get("If-Match"))
	if err != nil {
		http.Error(w, "If-Match with the current app ETag is required", http.StatusPreconditionRequired)
		return
	}
	if etagID != r.PathValue("id") {
		http.Error(w, "If-Match belongs to another app", http.StatusPreconditionFailed)
		return
	}
	if isNodeMutation(r) {
		state, _, loadErr := s.repo.Load(r.Context())
		current, ok := findApp(state.Apps, r.PathValue("id"))
		if loadErr != nil {
			writeStoreError(w, loadErr)
			return
		}
		if !ok || current.NodeID != s.identity.ID || (input.NodeID != "" && input.NodeID != s.identity.ID) {
			http.Error(w, "node credentials may only update local apps", http.StatusForbidden)
			return
		}
	}
	app, revision, err := s.repo.UpdateApp(r.Context(), r.PathValue("id"), generation, input, s.domain)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	s.reconciler.Trigger()
	w.Header().Set("ETag", appETag(app))
	writeRevision(w, revision)
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	etagID, generation, err := parseAppETag(r.Header.Get("If-Match"))
	if err != nil {
		http.Error(w, "If-Match with the current app ETag is required", http.StatusPreconditionRequired)
		return
	}
	if etagID != r.PathValue("id") {
		http.Error(w, "If-Match belongs to another app", http.StatusPreconditionFailed)
		return
	}
	if isNodeMutation(r) {
		state, _, loadErr := s.repo.Load(r.Context())
		current, ok := findApp(state.Apps, r.PathValue("id"))
		if loadErr != nil {
			writeStoreError(w, loadErr)
			return
		}
		if !ok || current.NodeID != s.identity.ID {
			http.Error(w, "node credentials may only delete local apps", http.StatusForbidden)
			return
		}
	}
	revision, err := s.repo.DeleteApp(r.Context(), r.PathValue("id"), generation)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	s.reconciler.Trigger()
	writeRevision(w, revision)
	w.WriteHeader(http.StatusNoContent)
}

type legacyAppInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	AppType     string `json:"app_type"`
	RouteRule   string `json:"route_rule"`
	TargetURL   string `json:"target_url"`
	NoAuth      bool   `json:"no_auth"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

type legacyApp struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	AppType     string    `json:"app_type"`
	RouteRule   string    `json:"route_rule"`
	TargetURL   string    `json:"target_url"`
	NoAuth      bool      `json:"no_auth"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s *Server) listLegacyApps(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	legacy := make([]legacyApp, len(state.Apps))
	for index, app := range state.Apps {
		legacy[index] = toLegacyApp(app)
	}
	writeRevision(w, state.Revision)
	writeJSON(w, http.StatusOK, legacy)
}

func (s *Server) getLegacyApp(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	app, ok := findApp(state.Apps, r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeRevision(w, state.Revision)
	writeJSON(w, http.StatusOK, toLegacyApp(app))
}

func toLegacyApp(app App) legacyApp {
	routeRule := ""
	noAuth := false
	if len(app.Routes) > 0 {
		routeRule = app.Routes[0].Rule
		noAuth = app.Routes[0].Auth == AuthUpstream
	}
	return legacyApp{
		ID: app.ID, Name: app.Name, Description: app.Description, AppType: "fe",
		RouteRule: routeRule, TargetURL: app.TargetURL, NoAuth: noAuth, Enabled: app.Enabled,
		CreatedAt: app.CreatedAt, UpdatedAt: app.UpdatedAt,
	}
}

func (s *Server) createLegacyApp(w http.ResponseWriter, r *http.Request) {
	var legacy legacyAppInput
	if err := decodeJSON(r, &legacy); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	input, err := s.legacyInput(legacy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	app, revision, err := s.repo.CreateApp(r.Context(), input, s.domain)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	s.reconciler.Trigger()
	writeRevision(w, revision)
	writeJSON(w, http.StatusCreated, toLegacyApp(app))
}

func (s *Server) updateLegacyApp(w http.ResponseWriter, r *http.Request) {
	var legacy legacyAppInput
	if err := decodeJSON(r, &legacy); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	input, err := s.legacyInput(legacy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	current, ok := findApp(state.Apps, r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if isNodeMutation(r) && current.NodeID != s.identity.ID {
		http.Error(w, "node credentials may only update local apps", http.StatusForbidden)
		return
	}
	if len(current.Routes) != 1 {
		http.Error(w, "v1 cannot update a multi-route app", http.StatusConflict)
		return
	}
	input.NodeID = current.NodeID
	app, revision, err := s.repo.UpdateApp(r.Context(), current.ID, current.Generation, input, s.domain)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	s.reconciler.Trigger()
	writeRevision(w, revision)
	writeJSON(w, http.StatusOK, toLegacyApp(app))
}

func (s *Server) deleteLegacyApp(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.repo.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	app, ok := findApp(state.Apps, r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if isNodeMutation(r) && app.NodeID != s.identity.ID {
		http.Error(w, "node credentials may only delete local apps", http.StatusForbidden)
		return
	}
	revision, err := s.repo.DeleteApp(r.Context(), app.ID, app.Generation)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	s.reconciler.Trigger()
	writeRevision(w, revision)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) legacyInput(legacy legacyAppInput) (AppInput, error) {
	if legacy.AppType == "db" {
		return AppInput{}, errors.New("database sidecars are no longer supported; register an HTTP service")
	}
	if legacy.AppType != "" && legacy.AppType != "fe" && legacy.AppType != "be" {
		return AppInput{}, errors.New("app_type must be fe or be")
	}
	if legacy.TargetURL == "" {
		return AppInput{}, errors.New("target_url is required; backend registrations now proxy directly")
	}
	auth := AuthBrowser
	if legacy.NoAuth {
		auth = AuthUpstream
	}
	routes := []RouteInput{}
	if legacy.RouteRule != "" {
		routes = append(routes, RouteInput{Rule: legacy.RouteRule, Auth: auth})
	}
	return AppInput{
		NodeID:      s.identity.ID,
		Name:        legacy.Name,
		Description: legacy.Description,
		TargetURL:   legacy.TargetURL,
		Routes:      routes,
		Enabled:     legacy.Enabled,
	}, nil
}

func serveFrontend(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := frontend.ReadFile("index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func decodeJSON(r *http.Request, value any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeMutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrPrecondition):
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
	case errors.Is(err, ErrValidation):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeStoreError(w, err)
	}
}

func writeStoreError(w http.ResponseWriter, err error) {
	log.Printf("registry storage: %v", err)
	http.Error(w, "registry state unavailable", http.StatusServiceUnavailable)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeRevision(w http.ResponseWriter, revision uint64) {
	w.Header().Set("X-Registry-Revision", fmt.Sprintf("%d", revision))
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
