package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrPrecondition = errors.New("precondition failed")
	ErrConflict     = errors.New("conflict")
	ErrValidation   = errors.New("validation failed")
	ErrNoChange     = errors.New("no change")
)

type StoredObject struct {
	Body []byte
	ETag string
}

type PutOptions struct {
	ContentType string
	IfMatch     string
	IfNoneMatch bool
}

type ObjectStore interface {
	Get(context.Context, string) (StoredObject, error)
	Put(context.Context, string, []byte, PutOptions) (string, error)
}

type MemoryObjectStore struct {
	mu      sync.Mutex
	objects map[string]StoredObject
}

func NewMemoryObjectStore() *MemoryObjectStore {
	return &MemoryObjectStore{objects: make(map[string]StoredObject)}
}

func (s *MemoryObjectStore) Get(_ context.Context, key string) (StoredObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	if !ok {
		return StoredObject{}, ErrNotFound
	}
	return StoredObject{Body: append([]byte(nil), object.Body...), ETag: object.ETag}, nil
}

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
