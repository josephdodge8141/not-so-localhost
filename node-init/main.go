// Package main initializes a persistent NSL node identity and enrollment.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Identity struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

type Enrollment struct {
	NodeID         string `json:"node_id"`
	NodeName       string `json:"node_name"`
	NodeCredential string `json:"node_credential"`
	Tunnel         struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	} `json:"tunnel"`
	PortalTunnel struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	} `json:"portal_tunnel"`
}

var invalidSlugCharacters = regexp.MustCompile(`[^a-z0-9-]+`)

func main() {
	stateDir := getEnv("NODE_STATE_DIR", "/var/lib/nsl")
	nodeName := strings.TrimSpace(os.Getenv("NODE_NAME"))
	if nodeName == "" {
		log.Fatal("NODE_NAME is required")
	}
	identityPath := filepath.Join(stateDir, "node.json")
	identity, err := loadOrCreateIdentity(identityPath, nodeName)
	if err != nil {
		log.Fatal(err)
	}

	credentialPath := filepath.Join(stateDir, "node-credential")
	nodeTunnelPath := filepath.Join(stateDir, "node-tunnel-token")
	portalTunnelPath := filepath.Join(stateDir, "portal-tunnel-token")
	if filesExist(credentialPath, nodeTunnelPath, portalTunnelPath) {
		log.Printf("node %s (%s) already enrolled", identity.Name, identity.ID)
		return
	}

	brokerURL := strings.TrimRight(os.Getenv("ENROLLMENT_BROKER_URL"), "/")
	token := os.Getenv("NSL_ENROLLMENT_TOKEN")
	if brokerURL == "" || token == "" {
		log.Fatal("ENROLLMENT_BROKER_URL and NSL_ENROLLMENT_TOKEN are required for first startup")
	}
	enrollment, err := enroll(context.Background(), brokerURL, token, identity)
	if err != nil {
		log.Fatal(err)
	}
	if enrollment.NodeID != identity.ID || enrollment.NodeName != identity.Slug {
		log.Fatal("broker returned a different node identity")
	}
	for file, value := range map[string]string{
		credentialPath:   enrollment.NodeCredential,
		nodeTunnelPath:   enrollment.Tunnel.Token,
		portalTunnelPath: enrollment.PortalTunnel.Token,
	} {
		if value == "" {
			log.Fatalf("broker response omitted %s", filepath.Base(file))
		}
		if err := atomicWrite(file, []byte(value+"\n"), 0600); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("enrolled node %s (%s)", identity.Name, identity.ID)
}

func enroll(ctx context.Context, brokerURL, token string, identity Identity) (Enrollment, error) {
	body, err := json.Marshal(map[string]string{
		"node_id": identity.ID, "node_name": identity.Slug, "enrollment_token": token,
	})
	if err != nil {
		return Enrollment{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, brokerURL+"/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return Enrollment{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return Enrollment{}, fmt.Errorf("enroll node: %w", err)
	}
	defer response.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Enrollment{}, err
	}
	if response.StatusCode != http.StatusOK {
		return Enrollment{}, fmt.Errorf("enrollment broker returned %d: %s", response.StatusCode, strings.TrimSpace(string(limited)))
	}
	var enrollment Enrollment
	if err := json.Unmarshal(limited, &enrollment); err != nil {
		return Enrollment{}, fmt.Errorf("decode enrollment: %w", err)
	}
	return enrollment, nil
}

func loadOrCreateIdentity(path, name string) (Identity, error) {
	slug := slugify(name)
	if slug == "" {
		return Identity{}, errors.New("NODE_NAME must contain a letter or number")
	}
	body, err := os.ReadFile(path)
	if err == nil {
		var identity Identity
		if err := json.Unmarshal(body, &identity); err != nil {
			return Identity{}, err
		}
		if identity.Name != name || identity.Slug != slug {
			return Identity{}, fmt.Errorf("NODE_NAME %q does not match persisted node %q", name, identity.Name)
		}
		return identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	id, err := randomUUID()
	if err != nil {
		return Identity{}, err
	}
	identity := Identity{ID: id, Name: name, Slug: slug, CreatedAt: time.Now().UTC()}
	body, err = json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return Identity{}, err
	}
	if err := atomicWrite(path, append(body, '\n'), 0600); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func filesExist(paths ...string) bool {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			return false
		}
	}
	return true
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Trim(invalidSlugCharacters.ReplaceAllString(value, "-"), "-")
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:]), nil
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".nsl-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
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
	return os.Rename(name, path)
}

func getEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
