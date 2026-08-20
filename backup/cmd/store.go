// Package main implements the backup service: periodic pg dumps pushed to S3
// (the store of record), local emergency copies while S3 is unreachable, and
// unconditional 30-day pruning of stale and orphaned backups.
package main

import (
	"context"
	"io"
)

// Store is a destination for backup blobs, keyed by object key.
type Store interface {
	Name() string
	Save(ctx context.Context, key string, r io.Reader) error
	Load(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	DeletePrefix(ctx context.Context, prefix string) error
	List(ctx context.Context, prefix string) ([]string, error)
}
