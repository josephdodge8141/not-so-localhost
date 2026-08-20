package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

func (s *FileStore) Save(_ context.Context, key string, r io.Reader) error {
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

func (s *FileStore) Load(_ context.Context, key string) (io.ReadCloser, error) {
	path := filepath.Join(s.baseDir, key)
	return os.Open(path)
}

func (s *FileStore) Delete(ctx context.Context, key string) error {
	path := filepath.Join(s.baseDir, key)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *FileStore) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		return fmt.Errorf("refusing to delete empty prefix")
	}
	path := filepath.Join(s.baseDir, prefix)
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	s.removeEmptyDirs()
	return nil
}

func (s *FileStore) List(ctx context.Context, prefix string) ([]string, error) {
	root := filepath.Join(s.baseDir, prefix)
	var keys []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.baseDir, path)
		if err != nil {
			return err
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *FileStore) removeEmptyDirs() {
	var dirs []string
	filepath.WalkDir(s.baseDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != s.baseDir {
			dirs = append(dirs, path)
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
		}
	}
}

func prefixOfKey(key string) string {
	rel := strings.TrimPrefix(key, "backups/")
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}