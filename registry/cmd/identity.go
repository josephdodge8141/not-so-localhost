// Package main runs the distributed Not-So-Localhost app registry.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// NodeIdentity is the persistent identity stored in a node's credential volume.
type NodeIdentity struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

var invalidSlugCharacters = regexp.MustCompile(`[^a-z0-9-]+`)

// LoadOrCreateIdentity loads the persisted identity or creates it atomically.
func LoadOrCreateIdentity(path, nodeName string) (NodeIdentity, error) {
	nodeName = strings.TrimSpace(nodeName)
	slug := slugify(nodeName)
	if slug == "" || len(slug) > 30 {
		return NodeIdentity{}, errors.New("NODE_NAME must contain a letter or number")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var identity NodeIdentity
		if err := json.Unmarshal(data, &identity); err != nil {
			return NodeIdentity{}, fmt.Errorf("decode node identity: %w", err)
		}
		if identity.Name != nodeName || identity.Slug != slug {
			return NodeIdentity{}, fmt.Errorf("NODE_NAME %q does not match persisted node %q", nodeName, identity.Name)
		}
		return identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return NodeIdentity{}, fmt.Errorf("read node identity: %w", err)
	}

	id, err := randomUUID()
	if err != nil {
		return NodeIdentity{}, err
	}
	identity := NodeIdentity{ID: id, Name: nodeName, Slug: slug, CreatedAt: time.Now().UTC()}
	body, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return NodeIdentity{}, fmt.Errorf("encode node identity: %w", err)
	}
	if err := atomicWrite(path, append(body, '\n'), 0600); err != nil {
		return NodeIdentity{}, fmt.Errorf("persist node identity: %w", err)
	}
	return identity, nil
}

func slugify(value string) string {
	slug := strings.ToLower(strings.TrimSpace(value))
	slug = invalidSlugCharacters.ReplaceAllString(slug, "-")
	return strings.Trim(slug, "-")
}

func randomUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:]), nil
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".nsl-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}
