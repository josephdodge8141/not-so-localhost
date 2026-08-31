// Package main runs the distributed Not-So-Localhost app registry.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
)

var (
	// ErrNotFound indicates that an object or registry entity does not exist.
	ErrNotFound = errors.New("not found")
	// ErrPrecondition indicates a failed conditional write or stale generation.
	ErrPrecondition = errors.New("precondition failed")
	// ErrConflict indicates a conflicting node, app, or route.
	ErrConflict = errors.New("conflict")
	// ErrValidation indicates invalid input.
	ErrValidation = errors.New("validation failed")
	// ErrNoChange indicates an idempotent mutation.
	ErrNoChange = errors.New("no change")
)

// StoredObject contains object bytes and their concurrency token.
type StoredObject struct {
	Body []byte
	ETag string
}

// PutOptions configures a conditional object write.
type PutOptions struct {
	ContentType string
	IfMatch     string
	IfNoneMatch bool
}

// ObjectStore provides the registry's conditional object operations.
type ObjectStore interface {
	Get(context.Context, string) (StoredObject, error)
	Put(context.Context, string, []byte, PutOptions) (string, error)
}

// MemoryObjectStore is an in-memory ObjectStore used by tests.
type MemoryObjectStore struct {
	mu      sync.Mutex
	objects map[string]StoredObject
}

// NewMemoryObjectStore creates an empty in-memory object store.
func NewMemoryObjectStore() *MemoryObjectStore {
	return &MemoryObjectStore{objects: make(map[string]StoredObject)}
}

// Get returns a copy of the object stored at key.
func (s *MemoryObjectStore) Get(_ context.Context, key string) (StoredObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	if !ok {
		return StoredObject{}, ErrNotFound
	}
	return StoredObject{Body: append([]byte(nil), object.Body...), ETag: object.ETag}, nil
}

// Put conditionally writes an object and returns its ETag.
func (s *MemoryObjectStore) Put(_ context.Context, key string, body []byte, options PutOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.objects[key]
	if options.IfNoneMatch && exists {
		return "", ErrPrecondition
	}
	if options.IfMatch != "" && (!exists || current.ETag != options.IfMatch) {
		return "", ErrPrecondition
	}
	etag := bodyETag(body)
	s.objects[key] = StoredObject{Body: append([]byte(nil), body...), ETag: etag}
	return etag, nil
}

func bodyETag(body []byte) string {
	hash := sha256.Sum256(body)
	return `"` + hex.EncodeToString(hash[:]) + `"`
}
