package main

import "time"

const stateSchemaVersion = 1

type AuthPolicy string

const (
	AuthBrowser  AuthPolicy = "browser"
	AuthUpstream AuthPolicy = "upstream"
)

type Node struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Roles     []string  `json:"roles,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Route struct {
	ID       string     `json:"id"`
	Rule     string     `json:"rule"`
	Priority int        `json:"priority"`
	Auth     AuthPolicy `json:"auth"`
}

type App struct {
	ID          string    `json:"id"`
	NodeID      string    `json:"node_id"`
	Generation  uint64    `json:"generation"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description,omitempty"`
	TargetURL   string    `json:"target_url"`
	PublicURL   string    `json:"public_url"`
	Routes      []Route   `json:"routes"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type RouteInput struct {
	Rule     string     `json:"rule"`
	Priority int        `json:"priority,omitempty"`
	Auth     AuthPolicy `json:"auth"`
}

type AppInput struct {
	NodeID      string       `json:"node_id,omitempty"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	TargetURL   string       `json:"target_url"`
	Routes      []RouteInput `json:"routes,omitempty"`
	Enabled     *bool        `json:"enabled,omitempty"`
}

type RegistryState struct {
	SchemaVersion    int       `json:"schema_version"`
	Revision         uint64    `json:"revision"`
	UpdatedAt        time.Time `json:"updated_at"`
	AuthOwnerNodeID  string    `json:"auth_owner_node_id,omitempty"`
	RecentOperations []string  `json:"recent_operations,omitempty"`
	Nodes            []Node    `json:"nodes"`
	Apps             []App     `json:"apps"`
}

type SnapshotPointer struct {
	SchemaVersion int       `json:"schema_version"`
	Revision      uint64    `json:"revision"`
	SnapshotKey   string    `json:"snapshot_key"`
	SHA256        string    `json:"sha256"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func filterApps(apps []App, include func(App) bool) []App {
	filtered := make([]App, 0, len(apps))
	for _, app := range apps {
		if include(app) {
			filtered = append(filtered, app)
		}
	}
	return filtered
}

func findApp(apps []App, id string) (App, bool) {
	for _, app := range apps {
		if app.ID == id {
			return app, true
		}
	}
	return App{}, false
}
