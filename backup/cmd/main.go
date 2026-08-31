// Package main backs up node-local databases to node-namespaced object keys.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Config struct {
	BackupInterval        time.Duration
	S3Bucket              string
	AWSRegion             string
	BackupDir             string
	NodeIdentityFile      string
	TargetsFile           string
	APIToken              string
	KeycloakDBPassword    string
	PostgresAdminUser     string
	PostgresAdminPassword string
}

type NodeIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type TargetConfig struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	User        string `json:"user"`
	PasswordEnv string `json:"password_env"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
}

var targetIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62})$`)

type backupTarget struct {
	DBInfo
	ID    string
	Label string
}

type BackupRecord struct {
	TargetID      string     `json:"target_id"`
	DisplayName   string     `json:"display_name"`
	LastBackupAt  *time.Time `json:"last_backup_at"`
	LastBackupKey string     `json:"last_backup_key"`
	BackupCount   int        `json:"backup_count"`
	S3Prefix      string     `json:"s3_prefix"`
}

type Server struct {
	cfg      Config
	node     NodeIdentity
	stores   []Store
	dumpers  *DumperRegistry
	targets  []backupTarget
	locksMu  sync.Mutex
	locks    map[string]*sync.Mutex
	recordMu sync.RWMutex
	records  map[string]BackupRecord
}

func main() {
	cfg := loadConfig()
	if cfg.APIToken == "" {
		log.Fatal("BACKUP_API_TOKEN is required")
	}
	node, err := loadNodeIdentity(cfg.NodeIdentityFile)
	if err != nil {
		log.Fatal(err)
	}
	targets, err := loadTargets(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	stores, err := configureStores(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	dumpers := NewDumperRegistry()
	dumpers.Register(&PostgresDumper{AdminUser: cfg.PostgresAdminUser})

	server := &Server{
		cfg: cfg, node: node, stores: stores, dumpers: dumpers, targets: targets,
		locks: make(map[string]*sync.Mutex), records: make(map[string]BackupRecord),
	}
	if err := server.refreshRecords(ctx); err != nil {
		log.Printf("load backup history: %v", err)
	}
	go server.schedule(ctx)
	go func() {
		time.Sleep(5 * time.Second)
		server.runAll(ctx)
	}()

	mux := http.NewServeMux()
	mux.Handle("GET /api/backups", server.authenticate(http.HandlerFunc(server.handleList)))
	mux.Handle("POST /api/backups/{target}/backup", server.authenticate(http.HandlerFunc(server.handleBackup)))
	mux.Handle("POST /api/backups/{target}/restore", server.authenticate(http.HandlerFunc(server.handleRestore)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	httpServer := &http.Server{
		Addr: ":7273", Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Minute, IdleTimeout: 60 * time.Second,
	}
	log.Printf("backup on :7273 (node %s, %d targets)", node.Name, len(targets))
	log.Fatal(httpServer.ListenAndServe())
}

func loadConfig() Config {
	interval, err := time.ParseDuration(getEnv("BACKUP_INTERVAL", "1h"))
	if err != nil {
		log.Fatalf("BACKUP_INTERVAL: %v", err)
	}
	return Config{
		BackupInterval: interval, S3Bucket: os.Getenv("BACKUP_S3_BUCKET"),
		AWSRegion: getEnv("AWS_REGION", "us-east-1"), BackupDir: getEnv("BACKUP_DIR", "/backups"),
		NodeIdentityFile: getEnv("NODE_IDENTITY_FILE", "/var/lib/nsl/node.json"),
		TargetsFile:      getEnv("BACKUP_TARGETS_FILE", "/etc/nsl/backup-targets.json"),
		APIToken:         os.Getenv("BACKUP_API_TOKEN"), KeycloakDBPassword: os.Getenv("KEYCLOAK_DB_PASSWORD"),
		PostgresAdminUser:     getEnv("POSTGRES_ADMIN_USER", "admin"),
		PostgresAdminPassword: os.Getenv("POSTGRES_ADMIN_PASSWORD"),
	}
}

func configureStores(ctx context.Context, cfg Config) ([]Store, error) {
	var stores []Store
	if cfg.BackupDir != "" {
		stores = append(stores, NewFileStore(cfg.BackupDir))
	}
	if cfg.S3Bucket != "" {
		awsConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
		if err != nil {
			return nil, fmt.Errorf("aws config: %w", err)
		}
		stores = append(stores, NewS3Store(s3.NewFromConfig(awsConfig), cfg.S3Bucket))
	}
	if len(stores) == 0 {
		return nil, errors.New("set BACKUP_S3_BUCKET and/or BACKUP_DIR")
	}
	return stores, nil
}

func loadNodeIdentity(file string) (NodeIdentity, error) {
	body, err := os.ReadFile(file)
	if err != nil {
		return NodeIdentity{}, fmt.Errorf("read node identity: %w", err)
	}
	var identity NodeIdentity
	if err := json.Unmarshal(body, &identity); err != nil {
		return NodeIdentity{}, fmt.Errorf("decode node identity: %w", err)
	}
	if identity.ID == "" || identity.Name == "" {
		return NodeIdentity{}, errors.New("node identity is incomplete")
	}
	return identity, nil
}

func loadTargets(cfg Config) ([]backupTarget, error) {
	var targets []backupTarget
	if cfg.KeycloakDBPassword != "" {
		targets = append(targets, backupTarget{
			ID: "keycloak", Label: "Keycloak",
			DBInfo: DBInfo{Type: "postgres", Name: "keycloak", User: "keycloak", Password: cfg.KeycloakDBPassword, Host: "postgres", Port: 5432},
		})
	}
	body, err := os.ReadFile(cfg.TargetsFile)
	if errors.Is(err, os.ErrNotExist) {
		return targets, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read backup targets: %w", err)
	}
	var configured []TargetConfig
	if err := json.Unmarshal(body, &configured); err != nil {
		return nil, fmt.Errorf("decode backup targets: %w", err)
	}
	seen := map[string]bool{"keycloak": len(targets) > 0}
	for _, target := range configured {
		if !targetIDPattern.MatchString(target.ID) || seen[target.ID] {
			return nil, fmt.Errorf("backup target id %q is invalid or duplicated", target.ID)
		}
		seen[target.ID] = true
		port := target.Port
		if port == 0 {
			port = 5432
		}
		info := DBInfo{
			Type: target.Type, Name: target.Name, User: target.User, Host: target.Host, Port: port,
			Password: os.Getenv(target.PasswordEnv),
		}
		if info.Type == "" {
			info.Type = "postgres"
		}
		if info.Name == "" || info.User == "" || info.Host == "" || info.Password == "" {
			return nil, fmt.Errorf("backup target %q is missing connection configuration", target.ID)
		}
		label := target.Label
		if label == "" {
			label = target.ID
		}
		targets = append(targets, backupTarget{ID: target.ID, Label: label, DBInfo: info})
	}
	return targets, nil
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.cfg.APIToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) schedule(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.BackupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runAll(ctx)
		}
	}
}

func (s *Server) runAll(ctx context.Context) {
	for _, target := range s.targets {
		if err := s.backup(ctx, target); err != nil {
			log.Printf("backup %s: %v", target.Label, err)
		}
	}
}

func (s *Server) target(id string) (backupTarget, bool) {
	for _, target := range s.targets {
		if target.ID == id {
			return target, true
		}
	}
	return backupTarget{}, false
}

func (s *Server) targetLock(id string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if s.locks[id] == nil {
		s.locks[id] = &sync.Mutex{}
	}
	return s.locks[id]
}

func (s *Server) backup(ctx context.Context, target backupTarget) error {
	lock := s.targetLock(target.ID)
	lock.Lock()
	defer lock.Unlock()

	dumper := s.dumpers.Get(target.Type)
	if dumper == nil {
		return fmt.Errorf("no dumper for type %q", target.Type)
	}
	var dump bytes.Buffer
	if err := dumper.Dump(ctx, target.DBInfo, &dump); err != nil {
		return fmt.Errorf("dump: %w", err)
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := io.Copy(gzipWriter, &dump); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}

	timestamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	prefix := fmt.Sprintf("backups/v2/%s/%s", s.node.ID, target.ID)
	key := fmt.Sprintf("%s/%s.sql.gz", prefix, timestamp)
	s3Stores, fileStores := splitStores(s.stores)
	if len(s3Stores) > 0 {
		if err := promotePending(ctx, prefix, s3Stores, fileStores); err != nil {
			log.Printf("promote pending backups for %s: %v", target.ID, err)
		}
		uploaded := false
		for _, store := range s3Stores {
			if err := store.Save(ctx, key, bytes.NewReader(compressed.Bytes())); err != nil {
				log.Printf("save %s to %s: %v", target.ID, store.Name(), err)
				continue
			}
			uploaded = true
		}
		if !uploaded {
			savedLocally := false
			for _, store := range fileStores {
				if err := store.Save(ctx, key, bytes.NewReader(compressed.Bytes())); err != nil {
					log.Printf("save emergency backup to %s: %v", store.Name(), err)
				} else {
					savedLocally = true
				}
			}
			if !savedLocally {
				return errors.New("S3 upload and emergency local save both failed")
			}
			return errors.New("S3 upload failed; retained emergency local copy")
		}
		for _, store := range fileStores {
			_ = store.Delete(ctx, key)
		}
	} else {
		for _, store := range fileStores {
			if err := store.Save(ctx, key, bytes.NewReader(compressed.Bytes())); err != nil {
				return err
			}
		}
	}
	if err := s.refreshRecords(ctx); err != nil {
		return fmt.Errorf("refresh history: %w", err)
	}
	log.Printf("backup %s -> %s", target.ID, key)
	return nil
}

func promotePending(ctx context.Context, prefix string, s3Stores, fileStores []Store) error {
	for _, fileStore := range fileStores {
		keys, err := fileStore.List(ctx, prefix)
		if err != nil {
			return err
		}
		for _, key := range keys {
			reader, err := fileStore.Load(ctx, key)
			if err != nil {
				return err
			}
			body, readErr := io.ReadAll(reader)
			reader.Close()
			if readErr != nil {
				return readErr
			}
			uploaded := false
			for _, s3Store := range s3Stores {
				if err := s3Store.Save(ctx, key, bytes.NewReader(body)); err == nil {
					uploaded = true
				}
			}
			if !uploaded {
				return fmt.Errorf("could not upload %s", key)
			}
			if err := fileStore.Delete(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitStores(stores []Store) (s3Stores, fileStores []Store) {
	for _, store := range stores {
		if _, ok := store.(*S3Store); ok {
			s3Stores = append(s3Stores, store)
		} else {
			fileStores = append(fileStores, store)
		}
	}
	return s3Stores, fileStores
}

func (s *Server) historyStore() Store {
	for _, store := range s.stores {
		if _, ok := store.(*S3Store); ok {
			return store
		}
	}
	return s.stores[0]
}

func (s *Server) refreshRecords(ctx context.Context) error {
	root := fmt.Sprintf("backups/v2/%s/", s.node.ID)
	keySet := make(map[string]bool)
	var lastErr error
	for _, store := range s.stores {
		keys, err := store.List(ctx, root)
		if err != nil {
			lastErr = err
			continue
		}
		for _, key := range keys {
			keySet[key] = true
		}
	}
	if len(keySet) == 0 && lastErr != nil {
		return lastErr
	}
	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	byTarget := make(map[string][]string)
	for _, key := range keys {
		relative := strings.TrimPrefix(key, root)
		parts := strings.SplitN(relative, "/", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		byTarget[parts[0]] = append(byTarget[parts[0]], key)
	}
	records := make(map[string]BackupRecord)
	for targetID, targetKeys := range byTarget {
		sort.Strings(targetKeys)
		latest := targetKeys[len(targetKeys)-1]
		backupTime := parseBackupTime(latest)
		label := targetID
		if target, ok := s.target(targetID); ok {
			label = target.Label
		}
		records[targetID] = BackupRecord{
			TargetID: targetID, DisplayName: label, LastBackupAt: backupTime,
			LastBackupKey: latest, BackupCount: len(targetKeys), S3Prefix: root + targetID,
		}
	}
	s.recordMu.Lock()
	s.records = records
	s.recordMu.Unlock()
	return nil
}

func parseBackupTime(key string) *time.Time {
	value := strings.TrimSuffix(path.Base(key), ".sql.gz")
	parsed, err := time.Parse("20060102T150405.000000000Z", value)
	if err != nil {
		return nil
	}
	return &parsed
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if err := s.refreshRecords(r.Context()); err != nil {
		http.Error(w, "backup store unavailable", http.StatusServiceUnavailable)
		return
	}
	s.recordMu.RLock()
	records := make([]BackupRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	s.recordMu.RUnlock()
	sort.Slice(records, func(i, j int) bool { return records[i].TargetID < records[j].TargetID })
	writeJSON(w, records)
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	target, ok := s.target(r.PathValue("target"))
	if !ok {
		http.Error(w, "target not found", http.StatusNotFound)
		return
	}
	if err := s.backup(r.Context(), target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "target": target.ID})
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "restore is disabled until staged restore validation is implemented", http.StatusNotImplemented)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
