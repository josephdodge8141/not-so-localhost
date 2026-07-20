package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type FileStore struct {
	baseDir string
}

func NewFileStore(baseDir string) *FileStore {
	return &FileStore{baseDir: baseDir}
}

func (s *FileStore) Name() string {
	return fmt.Sprintf("file://%s", s.baseDir)
}

func (s *FileStore) Save(ctx context.Context, key string, r io.Reader) error {
	path := filepath.Join(s.baseDir, key)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (s *FileStore) Load(ctx context.Context, key string) (io.ReadCloser, error) {
	path := filepath.Join(s.baseDir, key)
	return os.Open(path)
}
