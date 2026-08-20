package main

import (
	"context"
	"io"
)

type Store interface {
	Name() string
	Save(ctx context.Context, key string, r io.Reader) error
	Load(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	DeletePrefix(ctx context.Context, prefix string) error
	List(ctx context.Context, prefix string) ([]string, error)
}
