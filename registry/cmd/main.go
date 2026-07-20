package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/lib/pq"
)

var (
	traefikRoutesDir = os.Getenv("TRAEFIK_ROUTES_DIR")
	routesFile       = "managed.yml"
	backupToken      = os.Getenv("BACKUP_TOKEN")
)

//go:embed index.html logviewer.html
var frontend embed.FS

type App struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	AppType          string    `json:"app_type"`
	RouteRule        string    `json:"route_rule"`
	TargetURL        string    `json:"target_url,omitempty"`
	DocsURL          string    `json:"docs_url,omitempty"`
	ConnectionString string    `json:"connection_string,omitempty"`
	ContainerName    string    `json:"container_name"`
	NoAuth           bool      `json:"no_auth"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://registry:changeme@localhost:5432/registry?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal("connect:", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatal("ping:", err)
	}

	migrate(db)

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("docker client: %v", err)
	}

	serv := &Server{db: db, docker: dockerClient}
	serv.ensureAllSidecars(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", serveFrontend)
	mux.HandleFunc("GET /api/apps", serv.listApps)
	mux.HandleFunc("POST /api/apps", serv.createApp)
	mux.HandleFunc("GET /api/apps/{id}", serv.getApp)
	mux.HandleFunc("PUT /api/apps/{id}", serv.updateApp)
	mux.HandleFunc("DELETE /api/apps/{id}", serv.deleteApp)
	mux.HandleFunc("GET /api/apps/{id}/logs", serv.getAppLogs)
	mux.HandleFunc("GET /logs/{id}", serveLogViewer)
	mux.HandleFunc("GET /internal/backup-targets", serv.backupTargets)

	log.Println("registry on :7272")
	log.Fatal(http.ListenAndServe(":7272", mux))
}

type Server struct {
	db     *sql.DB
	docker *client.Client
}

var (
	traefikNetwork  = "not-so-localhost_edge"
	internalNetwork = "not-so-localhost_internal"
)

func sanitizeName(name string) string {
	reg := regexp.MustCompile(`[^a-z0-9-]`)
	s := strings.ToLower(name)
	s = reg.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "app"
	}
	return s
}

func (s *Server) writeRouteFile(apps []App) {
	if traefikRoutesDir == "" {
		return
	}
	p := filepath.Join(traefikRoutesDir, routesFile)

	if len(apps) == 0 {
		os.Remove(p)
		log.Printf("removed %s (no managed apps)", p)
		return
	}

	var buf strings.Builder
	buf.WriteString("http:\n  routers:\n")
	for _, a := range apps {
		sn := sanitizeName(a.Name)
		rule := a.RouteRule
		if rule == "" {
			if a.AppType == "fe" {
				rule = fmt.Sprintf("Host(`apps.joedodge.dev`) && PathPrefix(`/%s`)", sn)
			} else {
				rule = fmt.Sprintf("Host(`%s.joedodge.dev`)", sn)
			}
		}
		mws := ""
		if !a.NoAuth {
			mws = "\n      middlewares:\n        - auth"
		}
		buf.WriteString(fmt.Sprintf("    %s:\n      rule: %q\n      priority: 10\n      entryPoints:\n        - web%s\n      service: %s\n", sn, rule, mws, sn))
	}

	buf.WriteString("  services:\n")
	for _, a := range apps {
		sn := sanitizeName(a.Name)
		var svcURL string
		switch a.AppType {
		case "fe":
			svcURL = a.TargetURL
		case "be":
			svcURL = fmt.Sprintf("http://%s-swagger:8080", sn)
		case "db":
			svcURL = fmt.Sprintf("http://%s-pgweb:8081", sn)
		}
		if svcURL == "" {
			continue
		}
		buf.WriteString(fmt.Sprintf("    %s:\n      loadBalancer:\n        servers:\n          - url: %q\n", sn, svcURL))
	}

	if err := os.WriteFile(p, []byte(buf.String()), 0644); err != nil {
		log.Printf("write route file: %v", err)
		return
	}
	log.Printf("wrote %d routes to %s", len(apps), p)
}

func (s *Server) writeAllRoutes() {
	rows, err := s.db.Query(`SELECT id, name, COALESCE(description,''), app_type, COALESCE(route_rule,''), COALESCE(target_url,''), COALESCE(docs_url,''), COALESCE(container_name,''), no_auth, enabled, created_at, updated_at FROM apps WHERE enabled = true`)
	if err != nil {
		log.Printf("writeAllRoutes query: %v", err)
		return
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.AppType, &a.RouteRule, &a.TargetURL, &a.DocsURL, &a.ContainerName, &a.NoAuth, &a.Enabled, &a.CreatedAt, &a.UpdatedAt); err != nil {
			log.Printf("writeAllRoutes scan: %v", err)
			continue
		}
		apps = append(apps, a)
	}
	s.writeRouteFile(apps)
}

func (s *Server) ensureImage(ctx context.Context, ref string) error {
	_, _, err := s.docker.ImageInspectWithRaw(ctx, ref)
	if err == nil {
		return nil
	}
	pull, err := s.docker.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	defer pull.Close()
	io.Copy(io.Discard, pull)
	return nil
}

func (s *Server) deploySidecar(ctx context.Context, app App) error {
	sn := sanitizeName(app.Name)

	var (
		imageRef string
		env      []string
	)

	switch app.AppType {
	case "be":
		imageRef = "swaggerapi/swagger-ui:latest"
		docURL := app.DocsURL
		if docURL == "" {
			docURL = app.TargetURL + "/swagger"
		}
		env = []string{"SWAGGER_JSON_URL=" + docURL}
	case "db":
		imageRef = "sosedoff/pgweb:latest"
		env = []string{"PGWEB_DATABASE_URL=" + app.ConnectionString}
	default:
		return nil
	}

	if err := s.ensureImage(ctx, imageRef); err != nil {
		return fmt.Errorf("ensure image %s: %w", imageRef, err)
	}

	contName := sn + "-swagger"
	if app.AppType == "db" {
		contName = sn + "-pgweb"
	}

	s.removeContainer(ctx, contName)

	cfg := &container.Config{
		Image: imageRef,
		Env:   env,
	}
	hostCfg := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
	}
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			traefikNetwork:  {},
			internalNetwork: {},
		},
	}

	cont, err := s.docker.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, contName)
	if err != nil {
		return fmt.Errorf("container create %s: %w", contName, err)
	}

	if err := s.docker.ContainerStart(ctx, cont.ID, container.StartOptions{}); err != nil {
		s.docker.ContainerRemove(ctx, cont.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("container start %s: %w", contName, err)
	}

	log.Printf("deployed sidecar %s (%s)", contName, cont.ID[:12])
	return nil
}

func (s *Server) removeSidecar(ctx context.Context, app App) {
	sn := sanitizeName(app.Name)
	for _, name := range []string{sn + "-swagger", sn + "-pgweb"} {
		s.removeContainer(ctx, name)
	}
}

func (s *Server) removeContainer(ctx context.Context, name string) {
	containers, err := s.docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", "^/?"+name+"$")),
	})
	if err != nil {
		return
	}
	for _, c := range containers {
		s.docker.ContainerStop(ctx, c.ID, container.StopOptions{})
		s.docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		log.Printf("removed container %s (%s)", name, c.ID[:12])
	}
}

func (s *Server) ensureAllSidecars(ctx context.Context) {
	rows, err := s.db.Query(`SELECT id, name, app_type, COALESCE(target_url,''), COALESCE(docs_url,''), COALESCE(connection_string,'') FROM apps WHERE enabled = true AND app_type IN ('be', 'db')`)
	if err != nil {
		log.Printf("ensureSidecars query: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.AppType, &a.TargetURL, &a.DocsURL, &a.ConnectionString); err != nil {
			log.Printf("ensureSidecars scan: %v", err)
			continue
		}
		if err := s.deploySidecar(ctx, a); err != nil {
			log.Printf("deploy sidecar %s: %v", a.Name, err)
		}
	}
	s.writeAllRoutes()
}

func serveFrontend(w http.ResponseWriter, r *http.Request) {
	data, err := frontend.ReadFile("index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	var (
		rows *sql.Rows
		err  error
	)
	appType := r.URL.Query().Get("type")
	q := `SELECT id, name, COALESCE(description,''), app_type, COALESCE(route_rule,''), COALESCE(target_url,''), COALESCE(docs_url,''), COALESCE(container_name,''), no_auth, enabled, created_at, updated_at, connection_string != '' FROM apps`
	if appType != "" {
		rows, err = s.db.Query(q+" WHERE app_type = $1 ORDER BY name", appType)
	} else {
		rows, err = s.db.Query(q + " ORDER BY name")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type safeApp struct {
		ID            string    `json:"id"`
		Name          string    `json:"name"`
		Description   string    `json:"description"`
		AppType       string    `json:"app_type"`
		RouteRule     string    `json:"route_rule"`
		TargetURL     string    `json:"target_url"`
		DocsURL       string    `json:"docs_url"`
		ContainerName string    `json:"container_name"`
		NoAuth        bool      `json:"no_auth"`
		Enabled       bool      `json:"enabled"`
		CreatedAt     time.Time `json:"created_at"`
		UpdatedAt     time.Time `json:"updated_at"`
		HasDB         bool      `json:"has_db"`
	}
	apps := []safeApp{}
	for rows.Next() {
		var a safeApp
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.AppType, &a.RouteRule, &a.TargetURL, &a.DocsURL, &a.ContainerName, &a.NoAuth, &a.Enabled, &a.CreatedAt, &a.UpdatedAt, &a.HasDB); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		apps = append(apps, a)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apps)
}

func validAppType(t string) bool {
	return t == "fe" || t == "be" || t == "db"
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var a App
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if a.Name == "" || a.AppType == "" {
		http.Error(w, "name and app_type required", http.StatusBadRequest)
		return
	}
	if !validAppType(a.AppType) {
		http.Error(w, "app_type must be fe, be, or db", http.StatusBadRequest)
		return
	}
	if a.AppType == "fe" && a.TargetURL == "" {
		http.Error(w, "target_url required for fe type", http.StatusBadRequest)
		return
	}
	if a.AppType == "be" && a.DocsURL == "" {
		http.Error(w, "docs_url required for be type", http.StatusBadRequest)
		return
	}
	if a.AppType == "db" && a.ConnectionString == "" {
		http.Error(w, "connection_string required for db type", http.StatusBadRequest)
		return
	}
	if a.RouteRule == "" {
		sn := sanitizeName(a.Name)
		if a.AppType == "fe" {
			a.RouteRule = fmt.Sprintf("Host(`apps.joedodge.dev`) && PathPrefix(`/%s`)", sn)
		} else {
			a.RouteRule = fmt.Sprintf("Host(`%s.joedodge.dev`)", sn)
		}
	}
	if a.ContainerName == "" {
		a.ContainerName = inferContainerName(a)
	}

	err := s.db.QueryRow(
		`INSERT INTO apps (name, description, app_type, route_rule, target_url, docs_url, connection_string, container_name, no_auth)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, enabled, created_at, updated_at`,
		a.Name, a.Description, a.AppType, a.RouteRule, a.TargetURL, a.DocsURL, a.ConnectionString, a.ContainerName, a.NoAuth,
	).Scan(&a.ID, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			http.Error(w, "name already in use", http.StatusConflict)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if a.AppType == "be" || a.AppType == "db" {
		if err := s.deploySidecar(context.Background(), a); err != nil {
			log.Printf("sidecar deploy %s: %v", a.Name, err)
		}
	}

	s.writeAllRoutes()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(a)
}

func (s *Server) updateApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var old App
	err := s.db.QueryRow(`SELECT id, name, app_type, COALESCE(target_url,''), COALESCE(docs_url,''), COALESCE(connection_string,''), COALESCE(container_name,'') FROM apps WHERE id = $1`, id).Scan(&old.ID, &old.Name, &old.AppType, &old.TargetURL, &old.DocsURL, &old.ConnectionString, &old.ContainerName)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var a App
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if a.Name == "" || a.AppType == "" {
		http.Error(w, "name and app_type required", http.StatusBadRequest)
		return
	}
	if !validAppType(a.AppType) {
		http.Error(w, "app_type must be fe, be, or db", http.StatusBadRequest)
		return
	}
	if a.AppType == "fe" && a.TargetURL == "" {
		http.Error(w, "target_url required for fe type", http.StatusBadRequest)
		return
	}
	if a.AppType == "be" && a.DocsURL == "" {
		http.Error(w, "docs_url required for be type", http.StatusBadRequest)
		return
	}
	if a.AppType == "db" && a.ConnectionString == "" {
		http.Error(w, "connection_string required for db type", http.StatusBadRequest)
		return
	}
	if a.RouteRule == "" {
		sn := sanitizeName(a.Name)
		if a.AppType == "fe" {
			a.RouteRule = fmt.Sprintf("Host(`apps.joedodge.dev`) && PathPrefix(`/%s`)", sn)
		} else {
			a.RouteRule = fmt.Sprintf("Host(`%s.joedodge.dev`)", sn)
		}
	}
	if a.ContainerName == "" {
		a.ContainerName = inferContainerName(a)
	}

	err = s.db.QueryRow(
		`UPDATE apps SET name=$1, description=$2, app_type=$3, route_rule=$4, target_url=$5, docs_url=$6, connection_string=$7, container_name=$8, no_auth=$9, updated_at=now()
		 WHERE id=$10 RETURNING id, enabled, created_at, updated_at`,
		a.Name, a.Description, a.AppType, a.RouteRule, a.TargetURL, a.DocsURL, a.ConnectionString, a.ContainerName, a.NoAuth, id,
	).Scan(&a.ID, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Redeploy sidecars if type changed or config changed
	needsRedeploy := old.AppType != a.AppType || old.DocsURL != a.DocsURL || old.ConnectionString != a.ConnectionString
	if needsRedeploy || a.AppType == "be" || a.AppType == "db" {
		s.removeSidecar(context.Background(), App{Name: old.Name})
	}
	if a.AppType == "be" || a.AppType == "db" {
		if err := s.deploySidecar(context.Background(), a); err != nil {
			log.Printf("sidecar deploy %s: %v", a.Name, err)
		}
	}

	s.writeAllRoutes()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(a)
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var name, appType string
	s.db.QueryRow(`SELECT name, app_type FROM apps WHERE id = $1`, id).Scan(&name, &appType)
	if name != "" && (appType == "be" || appType == "db") {
		s.removeSidecar(context.Background(), App{Name: name})
	}

	result, err := s.db.Exec(`DELETE FROM apps WHERE id=$1`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.writeAllRoutes()
	w.WriteHeader(http.StatusNoContent)
}

type BackupTarget struct {
	Name             string `json:"name"`
	ConnectionString string `json:"connection_string"`
}

func (s *Server) backupTargets(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Backup-Token")
	if backupToken != "" && token != backupToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := s.db.Query(`SELECT name, connection_string FROM apps WHERE app_type = 'db' AND enabled = true AND connection_string != ''`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	targets := []BackupTarget{}
	for rows.Next() {
		var t BackupTarget
		if err := rows.Scan(&t.Name, &t.ConnectionString); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		targets = append(targets, t)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(targets)
}

func inferContainerName(a App) string {
	switch a.AppType {
	case "fe":
		if a.TargetURL != "" {
			u, err := url.Parse(a.TargetURL)
			if err == nil && u.Hostname() != "" {
				return u.Hostname()
			}
		}
	case "be":
		if a.DocsURL != "" {
			u, err := url.Parse(a.DocsURL)
			if err == nil && u.Hostname() != "" {
				return u.Hostname()
			}
		}
	}
	return sanitizeName(a.Name) + "-app"
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var a App
	err := s.db.QueryRow(`SELECT id, name, COALESCE(description,''), app_type, COALESCE(route_rule,''), COALESCE(target_url,''), COALESCE(docs_url,''), COALESCE(container_name,''), no_auth, enabled, created_at, updated_at FROM apps WHERE id = $1`, id).Scan(&a.ID, &a.Name, &a.Description, &a.AppType, &a.RouteRule, &a.TargetURL, &a.DocsURL, &a.ContainerName, &a.NoAuth, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(a)
}

func (s *Server) getAppLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var containerName string
	err := s.db.QueryRow(`SELECT COALESCE(container_name,'') FROM apps WHERE id = $1`, id).Scan(&containerName)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if containerName == "" {
		http.Error(w, "container_name not set", http.StatusNotFound)
		return
	}

	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "200",
		Timestamps: true,
	}
	reader, err := s.docker.ContainerLogs(r.Context(), containerName, opts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	stdcopy.StdCopy(w, w, reader)
}

func serveLogViewer(w http.ResponseWriter, r *http.Request) {
	data, err := frontend.ReadFile("logviewer.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func migrate(db *sql.DB) {
	db.Exec("ALTER TABLE apps ADD COLUMN IF NOT EXISTS container_name TEXT NOT NULL DEFAULT ''")
}
