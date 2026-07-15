package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lib/pq"
)

//go:embed index.html
var frontend embed.FS

type App struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	PathPrefix    string    `json:"path_prefix"`
	Port          int       `json:"port"`
	AppType       string    `json:"app_type"`
	Technology    string    `json:"technology"`
	ContainerName string    `json:"container_name"`
	Metadata      string    `json:"metadata"`
	DeviceID      string    `json:"device_id"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
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

	pgwebProxy := httputil.NewSingleHostReverseProxy(mustURL("http://pgweb:8081"))
	pgwebEmail := os.Getenv("PGWEB_ALLOWED_EMAIL")
	appProxies := newAppProxyCache(db)

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", serveFrontend)
	mux.HandleFunc("GET /api/apps", listApps(db))
	mux.HandleFunc("POST /api/apps", createApp(db))
	mux.HandleFunc("PUT /api/apps/{id}", updateApp(db))
	mux.HandleFunc("DELETE /api/apps/{id}", deleteApp(db))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pgweb" || strings.HasPrefix(r.URL.Path, "/pgweb/") {
			if pgwebEmail == "" || r.Header.Get("X-Forwarded-Email") != pgwebEmail {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/pgweb")
			if r.URL.RawPath != "" {
				r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, "/pgweb")
			}
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
			pgwebProxy.ServeHTTP(w, r)
			return
		}
		appProxies.ServeHTTP(w, r)
	})

	log.Println("registry on :7272")
	log.Fatal(http.ListenAndServe(":7272", mux))
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

type appProxyCache struct {
	db *sql.DB
}

func newAppProxyCache(db *sql.DB) *appProxyCache {
	return &appProxyCache{db: db}
}

func (c *appProxyCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	prefix := "/" + parts[0]
	var containerName string
	var port int
	err := c.db.QueryRow(
		`SELECT container_name, port FROM apps WHERE path_prefix = $1 AND enabled = true`, prefix,
	).Scan(&containerName, &port)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	host := containerName
	if host == "" {
		host = "localhost"
	}
	target := fmt.Sprintf("%s:%d", host, port)
	proxy := httputil.NewSingleHostReverseProxy(mustURL("http://" + target))

	r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
	if r.URL.RawPath != "" {
		r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, prefix)
	}
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	proxy.ServeHTTP(w, r)
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

func listApps(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			rows *sql.Rows
			err  error
		)
		appType := r.URL.Query().Get("type")
		if appType != "" {
			rows, err = db.Query(
				`SELECT id, name, description, path_prefix, port, app_type, technology, container_name, metadata::text, device_id, enabled, created_at, updated_at FROM apps WHERE app_type = $1 ORDER BY name`, appType,
			)
		} else {
			rows, err = db.Query(
				`SELECT id, name, description, path_prefix, port, app_type, technology, container_name, metadata::text, device_id, enabled, created_at, updated_at FROM apps ORDER BY name`,
			)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		apps := []App{}
		for rows.Next() {
			var a App
			if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.PathPrefix, &a.Port, &a.AppType, &a.Technology, &a.ContainerName, &a.Metadata, &a.DeviceID, &a.Enabled, &a.CreatedAt, &a.UpdatedAt); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			apps = append(apps, a)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apps)
	}
}

func validAppType(t string) bool {
	return t == "frontend" || t == "backend" || t == "db"
}

func createApp(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var a App
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if a.Name == "" || a.PathPrefix == "" || a.Port == 0 || a.AppType == "" {
			http.Error(w, "name, path_prefix, port, app_type required", http.StatusBadRequest)
			return
		}
		if a.Port <= 0 || a.Port > 65535 {
			http.Error(w, "port must be between 1 and 65535", http.StatusBadRequest)
			return
		}
		if !validAppType(a.AppType) {
			http.Error(w, "app_type must be frontend, backend, or db", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(a.PathPrefix, "/") {
			http.Error(w, "path_prefix must start with /", http.StatusBadRequest)
			return
		}
		if a.PathPrefix == "/pgweb" || strings.HasPrefix(a.PathPrefix, "/pgweb/") {
			http.Error(w, "path_prefix /pgweb is reserved", http.StatusBadRequest)
			return
		}
		if a.Metadata == "" {
			a.Metadata = "{}"
		}
		err := db.QueryRow(
			`INSERT INTO apps (name, description, path_prefix, port, app_type, technology, container_name, metadata, device_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)
			 RETURNING id, enabled, created_at, updated_at`,
			a.Name, a.Description, a.PathPrefix, a.Port, a.AppType, a.Technology, a.ContainerName, a.Metadata, a.DeviceID,
		).Scan(&a.ID, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
		if err != nil {
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				http.Error(w, "path_prefix already in use", http.StatusConflict)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(a)
	}
}

func updateApp(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var a App
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if a.Name == "" || a.PathPrefix == "" || a.Port == 0 || a.AppType == "" {
			http.Error(w, "name, path_prefix, port, app_type required", http.StatusBadRequest)
			return
		}
		if a.Port <= 0 || a.Port > 65535 {
			http.Error(w, "port must be between 1 and 65535", http.StatusBadRequest)
			return
		}
		if !validAppType(a.AppType) {
			http.Error(w, "app_type must be frontend, backend, or db", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(a.PathPrefix, "/") {
			http.Error(w, "path_prefix must start with /", http.StatusBadRequest)
			return
		}
		if a.PathPrefix == "/pgweb" || strings.HasPrefix(a.PathPrefix, "/pgweb/") {
			http.Error(w, "path_prefix /pgweb is reserved", http.StatusBadRequest)
			return
		}
		var metadataArg interface{}
		if a.Metadata != "" {
			metadataArg = a.Metadata
		}
		err := db.QueryRow(
			`UPDATE apps SET name=$1, description=$2, path_prefix=$3, port=$4, app_type=$5, technology=$6, container_name=$7, metadata=COALESCE($8::jsonb, metadata), device_id=$9, updated_at=now()
			 WHERE id=$10 RETURNING id, enabled, created_at, updated_at`,
			a.Name, a.Description, a.PathPrefix, a.Port, a.AppType, a.Technology, a.ContainerName, metadataArg, a.DeviceID, id,
		).Scan(&a.ID, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				http.Error(w, "path_prefix already in use", http.StatusConflict)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(a)
	}
}

func deleteApp(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		result, err := db.Exec(`DELETE FROM apps WHERE id=$1`, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func migrate(db *sql.DB) {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS apps (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name TEXT NOT NULL,
			description TEXT DEFAULT '',
			path_prefix TEXT NOT NULL UNIQUE,
			port INTEGER NOT NULL,
			app_type TEXT NOT NULL CHECK (app_type IN ('frontend', 'backend', 'db')),
			technology TEXT DEFAULT '',
			container_name TEXT DEFAULT '',
			metadata JSONB DEFAULT '{}',
			device_id TEXT NOT NULL DEFAULT 'local',
			enabled BOOLEAN DEFAULT true,
			created_at TIMESTAMPTZ DEFAULT now(),
			updated_at TIMESTAMPTZ DEFAULT now()
		)
	`)
	if err != nil {
		log.Fatal("migration:", err)
	}
}
