package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Reconciler struct {
	repo            *Repository
	nodeID          string
	directory       string
	broker          *BrokerClient
	trigger         chan struct{}
	authOwner       bool
	authProvisioned bool
	brokerRevision  uint64
}

func NewReconciler(repo *Repository, nodeID, directory string, broker *BrokerClient, authOwner bool) *Reconciler {
	return &Reconciler{repo: repo, nodeID: nodeID, directory: directory, broker: broker, trigger: make(chan struct{}, 1), authOwner: authOwner}
}

func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.trigger:
		}
		if err := r.Reconcile(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "route reconciliation: %v\n", err)
		}
	}
}

func (r *Reconciler) Reconcile(ctx context.Context) error {
	if r.directory == "" {
		return nil
	}
	state, _, err := r.repo.Load(ctx)
	if err != nil {
		return err
	}
	apps := filterApps(state.Apps, func(app App) bool { return app.NodeID == r.nodeID && app.Enabled })
	if r.broker != nil {
		if r.authOwner && !r.authProvisioned {
			if err := r.broker.EnsureAuth(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "provision auth hostname: %v\n", err)
			} else {
				r.authProvisioned = true
			}
		}
		if r.brokerRevision != state.Revision {
			slugs := make([]string, len(apps))
			for index, app := range apps {
				slugs[index] = app.Slug
			}
			if err := r.broker.SyncApps(ctx, slugs); err != nil {
				fmt.Fprintf(os.Stderr, "sync app hostnames: %v\n", err)
			} else {
				r.brokerRevision = state.Revision
			}
		}
	}
	path := filepath.Join(r.directory, "managed.yml")
	if len(apps) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	body := renderTraefik(apps)
	current, err := os.ReadFile(path)
	if err == nil && bytesEqual(current, body) {
		return nil
	}
	return atomicWrite(path, body, 0644)
}

func renderTraefik(apps []App) []byte {
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	var builder strings.Builder
	builder.WriteString("http:\n  routers:\n")
	for _, app := range apps {
		serviceName := "app-" + strings.ReplaceAll(app.ID, "-", "")
		for _, route := range app.Routes {
			routerName := "route-" + strings.ReplaceAll(route.ID, "-", "")
			builder.WriteString("    " + routerName + ":\n")
			builder.WriteString("      rule: " + strconv.Quote(route.Rule) + "\n")
			builder.WriteString(fmt.Sprintf("      priority: %d\n", route.Priority))
			builder.WriteString("      entryPoints:\n        - web\n")
			if route.Auth == AuthBrowser {
				builder.WriteString("      middlewares:\n        - auth\n")
			}
			builder.WriteString("      service: " + serviceName + "\n")
		}
	}
	builder.WriteString("  services:\n")
	for _, app := range apps {
		serviceName := "app-" + strings.ReplaceAll(app.ID, "-", "")
		builder.WriteString("    " + serviceName + ":\n")
		builder.WriteString("      loadBalancer:\n        servers:\n")
		builder.WriteString("          - url: " + strconv.Quote(app.TargetURL) + "\n")
	}
	return []byte(builder.String())
}
