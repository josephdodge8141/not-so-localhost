package main

import (
	"context"
	"io"
)

type Store interface {
	Name() string
	Save(ctx context.Context, key string, r io.Reader) error
	Load(ctx context.Context, key string) (io.ReadCloser, error)
}
