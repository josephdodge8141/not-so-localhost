// Package main runs the distributed Not-So-Localhost app registry.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxMutationAttempts = 64

// Repository coordinates immutable snapshots through conditional object writes.
type Repository struct {
	store  ObjectStore
	prefix string
	now    func() time.Time
}

// NewRepository creates a repository under the supplied object key prefix.
func NewRepository(store ObjectStore, prefix string) *Repository {
	return &Repository{store: store, prefix: strings.TrimSuffix(prefix, "/"), now: func() time.Time { return time.Now().UTC() }}
}

func (r *Repository) currentKey() string {
	return r.prefix + "/current.json"
}

func (r *Repository) initializedKey() string {
	return r.prefix + "/initialized"
}

// Load retrieves and verifies the current registry snapshot.
func (r *Repository) Load(ctx context.Context) (RegistryState, string, error) {
	current, err := r.store.Get(ctx, r.currentKey())
	if errors.Is(err, ErrNotFound) {
		if _, markerErr := r.store.Get(ctx, r.initializedKey()); markerErr == nil {
			return RegistryState{}, "", errors.New("registry current pointer is missing after initialization")
		} else if !errors.Is(markerErr, ErrNotFound) {
			return RegistryState{}, "", markerErr
		}
		return RegistryState{SchemaVersion: stateSchemaVersion, Nodes: []Node{}, Apps: []App{}}, "", nil
	}
	if err != nil {
		return RegistryState{}, "", err
	}
	var pointer SnapshotPointer
	if err := json.Unmarshal(current.Body, &pointer); err != nil {
		return RegistryState{}, "", fmt.Errorf("decode current pointer: %w", err)
	}
	if pointer.SchemaVersion != stateSchemaVersion {
		return RegistryState{}, "", fmt.Errorf("unsupported pointer schema %d", pointer.SchemaVersion)
	}
	snapshot, err := r.store.Get(ctx, pointer.SnapshotKey)
	if err != nil {
		return RegistryState{}, "", fmt.Errorf("load snapshot: %w", err)
	}
	hash := sha256.Sum256(snapshot.Body)
	if hex.EncodeToString(hash[:]) != pointer.SHA256 {
		return RegistryState{}, "", errors.New("registry snapshot checksum mismatch")
	}
	var state RegistryState
	if err := json.Unmarshal(snapshot.Body, &state); err != nil {
		return RegistryState{}, "", fmt.Errorf("decode snapshot: %w", err)
	}
	if state.SchemaVersion != stateSchemaVersion || state.Revision != pointer.Revision {
		return RegistryState{}, "", errors.New("registry pointer and snapshot disagree")
	}
	return state, current.ETag, nil
}

func (r *Repository) mutate(ctx context.Context, apply func(*RegistryState) error) (RegistryState, error) {
	operationID, err := randomUUID()
	if err != nil {
		return RegistryState{}, err
	}
	for attempt := 0; attempt < maxMutationAttempts; attempt++ {
		current, currentETag, err := r.Load(ctx)
		if err != nil {
			return RegistryState{}, err
		}
		next := cloneState(current)
		if err := apply(&next); errors.Is(err, ErrNoChange) {
			return current, nil
		} else if err != nil {
			return RegistryState{}, err
		}
		next.SchemaVersion = stateSchemaVersion
		next.Revision = current.Revision + 1
		next.UpdatedAt = r.now()
		next.RecentOperations = append(next.RecentOperations, operationID)
		if len(next.RecentOperations) > 100 {
			next.RecentOperations = append([]string(nil), next.RecentOperations[len(next.RecentOperations)-100:]...)
		}
		sortState(&next)
		body, err := json.Marshal(next)
		if err != nil {
			return RegistryState{}, err
		}
		hash := sha256.Sum256(body)
		digest := hex.EncodeToString(hash[:])
		snapshotKey := fmt.Sprintf("%s/snapshots/%020d-%s.json", r.prefix, next.Revision, digest)
		_, err = r.store.Put(ctx, snapshotKey, body, PutOptions{ContentType: "application/json", IfNoneMatch: true})
		if errors.Is(err, ErrPrecondition) {
			existing, getErr := r.store.Get(ctx, snapshotKey)
			if getErr != nil || !bytesEqual(existing.Body, body) {
				return RegistryState{}, fmt.Errorf("snapshot key collision")
			}
		} else if err != nil {
			return RegistryState{}, err
		}
		pointer := SnapshotPointer{
			SchemaVersion: stateSchemaVersion,
			Revision:      next.Revision,
			SnapshotKey:   snapshotKey,
			SHA256:        digest,
			UpdatedAt:     next.UpdatedAt,
		}
		pointerBody, err := json.Marshal(pointer)
		if err != nil {
			return RegistryState{}, err
		}
		options := PutOptions{ContentType: "application/json", IfMatch: currentETag, IfNoneMatch: currentETag == ""}
		if _, err := r.store.Put(ctx, r.currentKey(), pointerBody, options); err != nil {
			if r.pointerMatches(ctx, snapshotKey, digest) || r.operationCommitted(ctx, operationID) {
				if currentETag == "" {
					if markerErr := r.ensureInitializedMarker(ctx); markerErr != nil {
						return RegistryState{}, markerErr
					}
				}
				return next, nil
			}
			if errors.Is(err, ErrPrecondition) {
				continue
			}
			return RegistryState{}, err
		}
		if currentETag == "" {
			if err := r.ensureInitializedMarker(ctx); err != nil {
				return RegistryState{}, err
			}
		}
		return next, nil
	}
	return RegistryState{}, fmt.Errorf("%w: registry changed too many times; retry", ErrConflict)
}

func (r *Repository) operationCommitted(ctx context.Context, operationID string) bool {
	state, _, err := r.Load(ctx)
	if err != nil {
		return false
	}
	for _, existing := range state.RecentOperations {
		if existing == operationID {
			return true
		}
	}
	return false
}

func (r *Repository) ensureInitializedMarker(ctx context.Context) error {
	_, err := r.store.Put(ctx, r.initializedKey(), []byte("initialized\n"), PutOptions{ContentType: "text/plain", IfNoneMatch: true})
	if errors.Is(err, ErrPrecondition) {
		return nil
	}
	return err
}

func (r *Repository) pointerMatches(ctx context.Context, snapshotKey, digest string) bool {
	object, err := r.store.Get(ctx, r.currentKey())
	if err != nil {
		return false
	}
	var pointer SnapshotPointer
	return json.Unmarshal(object.Body, &pointer) == nil && pointer.SnapshotKey == snapshotKey && pointer.SHA256 == digest
}

// RegisterNode adds or idempotently refreshes an enrolled node.
func (r *Repository) RegisterNode(ctx context.Context, identity NodeIdentity) error {
	_, err := r.mutate(ctx, func(state *RegistryState) error {
		for index, node := range state.Nodes {
			if node.Slug == identity.Slug && node.ID != identity.ID {
				return fmt.Errorf("%w: node name %q is already registered", ErrConflict, identity.Name)
			}
			if node.ID == identity.ID {
				if node.Name == identity.Name && node.Slug == identity.Slug {
					return ErrNoChange
				}
				state.Nodes[index].Name = identity.Name
				state.Nodes[index].Slug = identity.Slug
				state.Nodes[index].UpdatedAt = r.now()
				return nil
			}
		}
		now := r.now()
		state.Nodes = append(state.Nodes, Node{
			ID: identity.ID, Name: identity.Name, Slug: identity.Slug,
			CreatedAt: identity.CreatedAt, UpdatedAt: now,
		})
		return nil
	})
	return err
}

// ClaimAuthOwner assigns the singleton authentication role to a node.
func (r *Repository) ClaimAuthOwner(ctx context.Context, nodeID string) error {
	_, err := r.mutate(ctx, func(state *RegistryState) error {
		if _, ok := findNode(state.Nodes, nodeID); !ok {
			return fmt.Errorf("%w: auth owner node is not registered", ErrValidation)
		}
		if state.AuthOwnerNodeID != "" && state.AuthOwnerNodeID != nodeID {
			return fmt.Errorf("%w: auth is already owned by node %s", ErrConflict, state.AuthOwnerNodeID)
		}
		if state.AuthOwnerNodeID == nodeID {
			return ErrNoChange
		}
		state.AuthOwnerNodeID = nodeID
		for index := range state.Nodes {
			state.Nodes[index].Roles = removeString(state.Nodes[index].Roles, "auth")
			if state.Nodes[index].ID == nodeID {
				state.Nodes[index].Roles = append(state.Nodes[index].Roles, "auth")
			}
		}
		return nil
	})
	return err
}

// CreateApp registers a new application and its routes.
func (r *Repository) CreateApp(ctx context.Context, input AppInput, domain string) (App, uint64, error) {
	id, err := randomUUID()
	if err != nil {
		return App{}, 0, err
	}
	routeIDs := make([]string, max(1, len(input.Routes)))
	for index := range routeIDs {
		routeIDs[index], err = randomUUID()
		if err != nil {
			return App{}, 0, err
		}
	}
	now := r.now()
	var created App
	state, err := r.mutate(ctx, func(state *RegistryState) error {
		app, err := buildApp(*state, id, 1, now, now, input, routeIDs, domain)
		if err != nil {
			return err
		}
		if err := validateAppConflicts(*state, app, ""); err != nil {
			return err
		}
		state.Apps = append(state.Apps, app)
		created = app
		return nil
	})
	return created, state.Revision, err
}

// UpdateApp replaces an application when its generation still matches.
func (r *Repository) UpdateApp(ctx context.Context, id string, generation uint64, input AppInput, domain string) (App, uint64, error) {
	now := r.now()
	var updated App
	state, err := r.mutate(ctx, func(state *RegistryState) error {
		for index, current := range state.Apps {
			if current.ID != id {
				continue
			}
			if current.Generation != generation {
				return fmt.Errorf("%w: app has changed", ErrPrecondition)
			}
			if input.NodeID == "" {
				input.NodeID = current.NodeID
			}
			routeIDs := make([]string, max(1, len(input.Routes)))
			for routeIndex := range routeIDs {
				if routeIndex < len(current.Routes) {
					routeIDs[routeIndex] = current.Routes[routeIndex].ID
				} else {
					newID, err := randomUUID()
					if err != nil {
						return err
					}
					routeIDs[routeIndex] = newID
				}
			}
			app, err := buildApp(*state, current.ID, current.Generation+1, current.CreatedAt, now, input, routeIDs, domain)
			if err != nil {
				return err
			}
			if err := validateAppConflicts(*state, app, current.ID); err != nil {
				return err
			}
			state.Apps[index] = app
			updated = app
			return nil
		}
		return ErrNotFound
	})
	return updated, state.Revision, err
}

// DeleteApp removes an application when its generation still matches.
func (r *Repository) DeleteApp(ctx context.Context, id string, generation uint64) (uint64, error) {
	state, err := r.mutate(ctx, func(state *RegistryState) error {
		for index, app := range state.Apps {
			if app.ID != id {
				continue
			}
			if app.Generation != generation {
				return fmt.Errorf("%w: app has changed", ErrPrecondition)
			}
			state.Apps = append(state.Apps[:index], state.Apps[index+1:]...)
			return nil
		}
		return ErrNotFound
	})
	return state.Revision, err
}

func buildApp(state RegistryState, id string, generation uint64, createdAt, updatedAt time.Time, input AppInput, routeIDs []string, domain string) (App, error) {
	name := strings.TrimSpace(input.Name)
	slug := slugify(name)
	if name == "" || slug == "" || len(slug) > 30 {
		return App{}, fmt.Errorf("%w: name is required", ErrValidation)
	}
	if slug == "t" {
		return App{}, fmt.Errorf("%w: app name t is reserved for the node terminal", ErrValidation)
	}
	node, ok := findNode(state.Nodes, input.NodeID)
	if !ok {
		return App{}, fmt.Errorf("%w: node_id is not registered", ErrValidation)
	}
	target, err := url.Parse(strings.TrimSpace(input.TargetURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		return App{}, fmt.Errorf("%w: target_url must be an HTTP URL without credentials", ErrValidation)
	}
	hostname := fmt.Sprintf("%s--%s.%s", slug, node.Slug, domain)
	if len(slug)+2+len(node.Slug) > 63 {
		return App{}, fmt.Errorf("%w: app and node names exceed the DNS label limit", ErrValidation)
	}
	routeInputs := input.Routes
	if len(routeInputs) == 0 {
		routeInputs = []RouteInput{{Rule: fmt.Sprintf("Host(`%s`)", hostname), Auth: AuthBrowser}}
	}
	if len(routeInputs) > 8 {
		return App{}, fmt.Errorf("%w: an app may have at most 8 routes", ErrValidation)
	}
	routes := make([]Route, len(routeInputs))
	for index, routeInput := range routeInputs {
		rule := strings.TrimSpace(routeInput.Rule)
		expectedHost := fmt.Sprintf("Host(`%s`)", hostname)
		allowedRules := map[string]bool{
			expectedHost: true,
			expectedHost + " && !(Path(`/v1`) || PathPrefix(`/v1/`))": true,
			expectedHost + " && (Path(`/v1`) || PathPrefix(`/v1/`))":  true,
		}
		if !allowedRules[rule] {
			return App{}, fmt.Errorf("%w: route rule is invalid", ErrValidation)
		}
		if routeInput.Auth != AuthBrowser && routeInput.Auth != AuthUpstream {
			return App{}, fmt.Errorf("%w: route auth must be browser or upstream", ErrValidation)
		}
		priority := routeInput.Priority
		if priority == 0 {
			priority = 100 + index
		}
		if priority < 1 || priority > 1000 {
			return App{}, fmt.Errorf("%w: route priority must be between 1 and 1000", ErrValidation)
		}
		routes[index] = Route{ID: routeIDs[index], Rule: rule, Priority: priority, Auth: routeInput.Auth}
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	return App{
		ID: id, NodeID: input.NodeID, Generation: generation, Name: name, Slug: slug,
		Description: strings.TrimSpace(input.Description), TargetURL: target.String(),
		PublicURL: "https://" + hostname, Routes: routes, Enabled: enabled,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

func validateAppConflicts(state RegistryState, candidate App, excludeID string) error {
	seenRules := make(map[string]struct{}, len(candidate.Routes))
	for _, route := range candidate.Routes {
		if _, exists := seenRules[route.Rule]; exists {
			return fmt.Errorf("%w: duplicate route rule", ErrConflict)
		}
		seenRules[route.Rule] = struct{}{}
	}
	for _, app := range state.Apps {
		if app.ID == excludeID {
			continue
		}
		if app.NodeID == candidate.NodeID && app.Slug == candidate.Slug {
			return fmt.Errorf("%w: app name is already registered on this node", ErrConflict)
		}
		for _, existingRoute := range app.Routes {
			if _, exists := seenRules[existingRoute.Rule]; exists {
				return fmt.Errorf("%w: route rule is already registered", ErrConflict)
			}
		}
	}
	return nil
}

func findNode(nodes []Node, id string) (Node, bool) {
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

func cloneState(state RegistryState) RegistryState {
	clone := state
	clone.RecentOperations = append([]string(nil), state.RecentOperations...)
	clone.Nodes = append([]Node(nil), state.Nodes...)
	clone.Apps = make([]App, len(state.Apps))
	for index, app := range state.Apps {
		clone.Apps[index] = app
		clone.Apps[index].Routes = append([]Route(nil), app.Routes...)
	}
	return clone
}

func sortState(state *RegistryState) {
	sort.Slice(state.Nodes, func(i, j int) bool { return state.Nodes[i].ID < state.Nodes[j].ID })
	sort.Slice(state.Apps, func(i, j int) bool { return state.Apps[i].ID < state.Apps[j].ID })
	for index := range state.Apps {
		sort.Slice(state.Apps[index].Routes, func(i, j int) bool {
			return state.Apps[index].Routes[i].ID < state.Apps[index].Routes[j].ID
		})
	}
}

func removeString(values []string, target string) []string {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

func bytesEqual(left, right []byte) bool {
	return string(left) == string(right)
}

func appETag(app App) string {
	return strconv.Quote(fmt.Sprintf("app:%s:%d", app.ID, app.Generation))
}

func parseAppETag(value string) (string, uint64, error) {
	unquoted, err := strconv.Unquote(value)
	if err != nil {
		return "", 0, err
	}
	parts := strings.Split(unquoted, ":")
	if len(parts) != 3 || parts[0] != "app" {
		return "", 0, errors.New("invalid app ETag")
	}
	generation, err := strconv.ParseUint(parts[2], 10, 64)
	return parts[1], generation, err
}
