// Package main implements the backup service: periodic pg dumps pushed to S3
// (the store of record), local emergency copies while S3 is unreachable, and
// unconditional 30-day pruning of stale and orphaned backups.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds all runtime configuration for the backup service.
type Config struct {
	DatabaseURL           string
	RegistryURL           string
	BackupToken           string
	BackupInterval        time.Duration
	S3Bucket              string
	AWSRegion             string
	BackupDir             string
	KeycloakDBPassword    string
	RegistryDBPassword    string
	PostgresAdminPassword string
	StaleBackupDays       int
}

// Server owns the backup sweep: configured stores, the registry-backed
// target set, and the backup_tracker records.
type Server struct {
	cfg          Config
	db           *pgxpool.Pool
	stores       []Store
	dumpers      *DumperRegistry
	mu           sync.RWMutex
	records      map[string]*BackupRecord
	hardcodedDbs []HardcodedDB
}

// HardcodedDB describes a database backed up by name rather than via the
// registry, e.g. the stack databases themselves.
type HardcodedDB struct {
	Name     string
	User     string
	Password string
	DBName   string
}

// BackupRecord tracks the latest successful S3 backup for one database.
type BackupRecord struct {
	DBName        string     `json:"db_name"`
	DBUser        string     `json:"db_user"`
	DisplayName   string     `json:"display_name"`
	LastBackupAt  *time.Time `json:"last_backup_at"`
	LastBackupKey string     `json:"last_backup_key"`
	BackupCount   int        `json:"backup_count"`
	S3Prefix      string     `json:"s3_prefix"`
}

func main() {
	cfg := loadConfig()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS backup_tracker (
		db_name TEXT PRIMARY KEY,
		db_user TEXT NOT NULL,
		last_backup_at TIMESTAMPTZ,
		last_backup_key TEXT,
		backup_count INT DEFAULT 0,
		s3_prefix TEXT NOT NULL DEFAULT '',
		display_name TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		log.Fatalf("migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE backup_tracker ADD COLUMN IF NOT EXISTS display_name TEXT DEFAULT ''`); err != nil {
		log.Printf("add display_name col: %v", err)
	}

	dumpers := NewDumperRegistry()
	dumpers.Register(&PostgresDumper{AdminUser: "admin"})

	var stores []Store
	if cfg.BackupDir != "" {
		stores = append(stores, NewFileStore(cfg.BackupDir))
	}
	if cfg.S3Bucket != "" {
		s3cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
		if err != nil {
			log.Fatalf("aws config: %v", err)
		}
		stores = append(stores, NewS3Store(s3.NewFromConfig(s3cfg), cfg.S3Bucket))
	}
	if len(stores) == 0 {
		log.Fatalf("no stores configured: set BACKUP_DIR and/or BACKUP_S3_BUCKET")
	}

	s := &Server{
		cfg:     cfg,
		db:      pool,
		stores:  stores,
		dumpers: dumpers,
		records: make(map[string]*BackupRecord),
		hardcodedDbs: []HardcodedDB{
			{Name: "keycloak", User: "keycloak", Password: cfg.KeycloakDBPassword},
			{Name: "registry", User: "registry", Password: cfg.RegistryDBPassword},
		},
	}
	s.loadRecords(ctx)

	go s.scheduleBackups(ctx)

	go func() {
		time.Sleep(5 * time.Second)
		s.reconcileBackups(ctx)
		log.Println("startup stale cleanup")
		s.cleanupStaleBackups(ctx)
		log.Println("initial backup sweep")
		s.runAllBackups(ctx)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/backups", s.handleListBackups)
	mux.HandleFunc("/api/backups/{db}/backup", s.handleTriggerBackup)
	mux.HandleFunc("/api/backups/{db}/restore", s.handleRestore)

	log.Println("backup on :7273")
	log.Fatal(http.ListenAndServe(":7273", mux))
}

func loadConfig() Config {
	interval, err := time.ParseDuration(getEnv("BACKUP_INTERVAL", "1h"))
	if err != nil {
		interval = 1 * time.Hour
	}
	return Config{
		DatabaseURL:           getEnv("DATABASE_URL", "postgres://registry:registry@postgres:5432/registry"),
		RegistryURL:           getEnv("REGISTRY_URL", "http://registry:7272"),
		BackupInterval:        interval,
		S3Bucket:              getEnv("BACKUP_S3_BUCKET", ""),
		AWSRegion:             getEnv("AWS_REGION", "us-west-2"),
		BackupDir:             getEnv("BACKUP_DIR", "/backups"),
		KeycloakDBPassword:    os.Getenv("KEYCLOAK_DB_PASSWORD"),
		RegistryDBPassword:    os.Getenv("REGISTRY_DB_PASSWORD"),
		PostgresAdminPassword: os.Getenv("POSTGRES_ADMIN_PASSWORD"),
		BackupToken:           os.Getenv("BACKUP_TOKEN"),
		StaleBackupDays:       getEnvInt("STALE_BACKUP_DAYS", 30),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func (s *Server) databases(ctx context.Context) []backupTarget {
	seen := make(map[string]bool)
	var targets []backupTarget

	for _, h := range s.hardcodedDbs {
		if seen[h.Name] || h.Password == "" {
			continue
		}
		seen[h.Name] = true
		dbName := h.DBName
		if dbName == "" {
			dbName = h.Name
		}
		targets = append(targets, backupTarget{
			DBInfo: DBInfo{
				Type:     "postgres",
				Name:     dbName,
				User:     h.User,
				Password: h.Password,
				Host:     "postgres",
				Port:     5432,
			},
			Label: h.Name,
		})
	}

	discovered, err := s.discoverDBs(ctx)
	if err != nil {
		log.Printf("registry discovery: %v", err)
	} else {
		for _, d := range discovered {
			key := d.ID
			if key == "" {
				key = d.Label
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, d)
		}
	}

	return targets
}

type backupTarget struct {
	DBInfo
	Label string // display name
	ID    string // UUID from registry, empty for hardcoded DBs
}

func (s *Server) discoverDBs(ctx context.Context) ([]backupTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("%s/internal/backup-targets", s.cfg.RegistryURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Backup-Token", s.cfg.BackupToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned %d", resp.StatusCode)
	}

	var targets []struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		ConnectionString string `json:"connection_string"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, err
	}

	var result []backupTarget
	for _, t := range targets {
		result = append(result, backupTarget{
			DBInfo: DBInfo{
				Type:             "postgres",
				Name:             t.Name,
				ConnectionString: t.ConnectionString,
			},
			Label: t.Name,
			ID:    t.ID,
		})
	}
	return result, nil
}

func (s *Server) loadRecords(ctx context.Context) {
	rows, err := s.db.Query(ctx, "SELECT db_name, db_user, COALESCE(last_backup_at::timestamptz, 'epoch'::timestamptz), COALESCE(last_backup_key, ''), backup_count, s3_prefix, COALESCE(display_name, '') FROM backup_tracker")
	if err != nil {
		log.Printf("load records: %v", err)
		return
	}
	defer rows.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	for rows.Next() {
		var r BackupRecord
		var t time.Time
		if err := rows.Scan(&r.DBName, &r.DBUser, &t, &r.LastBackupKey, &r.BackupCount, &r.S3Prefix, &r.DisplayName); err != nil {
			log.Printf("scan record: %v", err)
			continue
		}
		if !t.IsZero() && !t.Equal(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)) {
			r.LastBackupAt = &t
		}
		s.records[r.DBName] = &r
	}
}

func (s *Server) upsertRecord(ctx context.Context, r *BackupRecord) error {
	s.mu.Lock()
	s.records[r.DBName] = r
	s.mu.Unlock()

	_, err := s.db.Exec(ctx, `INSERT INTO backup_tracker (db_name, db_user, last_backup_at, last_backup_key, backup_count, s3_prefix, display_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (db_name) DO UPDATE SET
			db_user = EXCLUDED.db_user,
			last_backup_at = EXCLUDED.last_backup_at,
			last_backup_key = EXCLUDED.last_backup_key,
			backup_count = EXCLUDED.backup_count,
			s3_prefix = EXCLUDED.s3_prefix,
			display_name = EXCLUDED.display_name`,
		r.DBName, r.DBUser, r.LastBackupAt, r.LastBackupKey, r.BackupCount, r.S3Prefix, r.DisplayName)
	return err
}

func (s *Server) scheduleBackups(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.BackupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Println("scheduled backup sweep")
			s.reconcileBackups(ctx)
			s.runAllBackups(ctx)
			s.cleanupStaleBackups(ctx)
		}
	}
}

func (s *Server) currentTargetKeys() map[string]bool {
	targets := s.databases(context.Background())
	keys := make(map[string]bool, len(targets))
	for _, t := range targets {
		k := t.ID
		if k == "" {
			k = t.Label
		}
		keys[k] = true
	}
	return keys
}

func parseKeyTime(key string) time.Time {
	base := path.Base(key)
	base = strings.TrimSuffix(base, ".sql.gz")
	t, err := time.Parse("20060102T150405Z", base)
	if err != nil {
		return time.Time{}
	}
	return t
}

// reconcileBackups enforces S3 as the store of record regardless of history:
//  1. Twin-clean: any local file that also exists in S3 is deleted locally.
//  2. Orphan-clean: prefixes with no tracker record and no current target
//     whose newest snapshot is older than StaleBackupDays are deleted from
//     every store.
func (s *Server) reconcileBackups(ctx context.Context) {
	current := s.currentTargetKeys()
	now := time.Now()

	var fileStores, s3Stores []Store
	for _, st := range s.stores {
		if _, ok := st.(*S3Store); ok {
			s3Stores = append(s3Stores, st)
		} else {
			fileStores = append(fileStores, st)
		}
	}

	var localKeys, s3Keys []string
	for _, st := range fileStores {
		keys, err := st.List(ctx, "backups")
		if err != nil {
			log.Printf("reconcile: list %s: %v", st.Name(), err)
		} else {
			localKeys = append(localKeys, keys...)
		}
	}
	for _, st := range s3Stores {
		keys, err := st.List(ctx, "backups")
		if err != nil {
			log.Printf("reconcile: list %s: %v", st.Name(), err)
		} else {
			s3Keys = append(s3Keys, keys...)
		}
	}
	s3Set := make(map[string]bool, len(s3Keys))
	for _, k := range s3Keys {
		s3Set[k] = true
	}

	removed := 0
	for _, k := range localKeys {
		if s3Set[k] {
			if err := fileStores[0].Delete(ctx, k); err != nil {
				log.Printf("reconcile: delete local twin %s: %v", k, err)
			} else {
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("reconcile: deleted %d local backups already present in S3", removed)
	}
	if fs, ok := fileStores[0].(*FileStore); ok && removed > 0 {
		fs.removeEmptyDirs()
	}

	type prefixInfo struct {
		newestKey string
	}
	prefixes := make(map[string]prefixInfo)
	addPrefixes := func(keys []string) {
		for _, k := range keys {
			p := prefixOfKey(k)
			if p == "" {
				continue
			}
			pi, ok := prefixes[p]
			if !ok || k > pi.newestKey {
				prefixes[p] = prefixInfo{newestKey: k}
			}
		}
	}
	addPrefixes(localKeys)
	addPrefixes(s3Keys)

	maxAge := time.Duration(s.cfg.StaleBackupDays) * 24 * time.Hour
	for p, pi := range prefixes {
		if current[p] {
			continue
		}
		newest := parseKeyTime(pi.newestKey)
		if newest.IsZero() || now.Sub(newest) < maxAge {
			continue
		}
		s.mu.RLock()
		_, tracked := s.records[p]
		s.mu.RUnlock()
		// Tracked-but-unregistered apps are handled by cleanupStaleBackups.
		if tracked {
			continue
		}
		age := now.Sub(newest)
		log.Printf("reconcile: pruning orphaned backups %q (newest %s, %s old)", p, path.Base(pi.newestKey), age.Round(time.Hour))
		for _, st := range s.stores {
			if err := st.DeletePrefix(ctx, fmt.Sprintf("backups/%s", p)); err != nil {
				log.Printf("reconcile: delete %s prefix %s: %v", st.Name(), p, err)
			}
		}
	}
}

func (s *Server) cleanupStaleBackups(ctx context.Context) {
	current := s.currentTargetKeys()
	maxAge := time.Duration(s.cfg.StaleBackupDays) * 24 * time.Hour
	now := time.Now()

	s.mu.RLock()
	var stale []string
	for dbKey, rec := range s.records {
		if current[dbKey] {
			continue
		}
		if rec.LastBackupAt == nil || rec.S3Prefix == "" {
			continue
		}
		if now.Sub(*rec.LastBackupAt) < maxAge {
			continue
		}
		stale = append(stale, dbKey)
	}
	s.mu.RUnlock()

	if len(stale) == 0 {
		return
	}

	log.Printf("cleaning %d stale backup(s) over %d days old", len(stale), s.cfg.StaleBackupDays)
	for _, dbKey := range stale {
		s.mu.RLock()
		rec := s.records[dbKey]
		s.mu.RUnlock()
		if rec == nil {
			continue
		}

		for _, store := range s.stores {
			if err := store.DeletePrefix(ctx, rec.S3Prefix); err != nil {
				log.Printf("delete %s %s: %v", store.Name(), rec.S3Prefix, err)
			}
		}

		if _, err := s.db.Exec(ctx, "DELETE FROM backup_tracker WHERE db_name = $1", dbKey); err != nil {
			log.Printf("delete tracker %s: %v", dbKey, err)
			continue
		}

		s.mu.Lock()
		delete(s.records, dbKey)
		s.mu.Unlock()

		display := rec.DisplayName
		if display == "" {
			display = dbKey
		}
		log.Printf("pruned stale backup: %s (%s)", display, dbKey)
	}
}

func (s *Server) runAllBackups(ctx context.Context) {
	targets := s.databases(ctx)
	for _, t := range targets {
		if err := s.backupTarget(ctx, t); err != nil {
			log.Printf("backup %s: %v", t.Label, err)
		}
	}
}

func (s *Server) backupTarget(ctx context.Context, t backupTarget) error {
	dumper := s.dumpers.Get(t.Type)
	if dumper == nil {
		return fmt.Errorf("no dumper for type %q", t.Type)
	}

	dbKey := t.ID
	if dbKey == "" {
		dbKey = t.Label
	}

	timestamp := time.Now().UTC().Format("20060102T150405Z")
	key := fmt.Sprintf("backups/%s/%s.sql.gz", dbKey, timestamp)
	prefix := fmt.Sprintf("backups/%s", dbKey)

	var dumpBuf bytes.Buffer
	if err := dumper.Dump(ctx, t.DBInfo, &dumpBuf); err != nil {
		return fmt.Errorf("dump: %w", err)
	}

	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write(dumpBuf.Bytes()); err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("gzip close: %w", err)
	}

	now := time.Now()
	rec := &BackupRecord{
		DBName:        dbKey,
		DBUser:        t.User,
		DisplayName:   t.Label,
		LastBackupKey: key,
		S3Prefix:      prefix,
	}
	s.mu.RLock()
	existing, exists := s.records[dbKey]
	s.mu.RUnlock()
	if exists {
		rec.BackupCount = existing.BackupCount + 1
	} else {
		rec.BackupCount = 1
	}

	// S3 is the store of record. Write there first; local storage is staging only.
	var s3Stores, fileStores []Store
	for _, st := range s.stores {
		if _, ok := st.(*S3Store); ok {
			s3Stores = append(s3Stores, st)
		} else {
			fileStores = append(fileStores, st)
		}
	}

	if len(s3Stores) > 0 {
		uploaded := false
		for _, st := range s3Stores {
			if err := st.Save(ctx, key, bytes.NewReader(compressed.Bytes())); err != nil {
				log.Printf("save %s -> %s: %v", t.Label, st.Name(), err)
				continue
			}
			uploaded = true
		}
		if !uploaded {
			// Nothing reached S3: hold an emergency local copy and do not
			// advance the record (LastBackupAt only tracks S3 success).
			for _, st := range fileStores {
				if err := st.Save(ctx, key, bytes.NewReader(compressed.Bytes())); err != nil {
					log.Printf("save %s -> %s (emergency): %v", t.Label, st.Name(), err)
				} else {
					log.Printf("S3 unavailable for %s; kept local emergency copy %s", t.Label, key)
				}
			}
			return fmt.Errorf("s3 save %s: no surviving S3 upload (local copy kept)", key)
		}
		// S3 upload succeeded: local is no longer needed, clear this prefix's
		// entire local history (any older local-only copies are superseded).
		for _, st := range fileStores {
			if err := st.DeletePrefix(ctx, prefix); err != nil {
				log.Printf("clear local %s after S3 upload: %v", dbKey, err)
			} else {
				log.Printf("s3 upload ok; cleared local backups for %s", dbKey)
			}
		}
		rec.LastBackupAt = &now
	} else {
		// Legacy config without S3: plain store loop, unchanged semantics.
		for _, st := range s.stores {
			if err := st.Save(ctx, key, bytes.NewReader(compressed.Bytes())); err != nil {
				log.Printf("save %s -> %s: %v", t.Label, st.Name(), err)
			}
		}
		rec.LastBackupAt = &now
	}

	if err := s.upsertRecord(ctx, rec); err != nil {
		log.Printf("track %s: %v", t.Label, err)
	}

	log.Printf("backup: %s -> %s/%s", t.Label, rec.S3Prefix, timestamp)
	return nil
}

func (s *Server) handleListBackups(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	records := make([]*BackupRecord, 0, len(s.records))
	for _, rec := range s.records {
		records = append(records, rec)
	}
	s.mu.RUnlock()

	sort.Slice(records, func(i, j int) bool {
		return records[i].DBName < records[j].DBName
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(records)
}

func (s *Server) handleTriggerBackup(w http.ResponseWriter, r *http.Request) {
	dbName := r.PathValue("db")
	if dbName == "" {
		http.Error(w, "missing db name", http.StatusBadRequest)
		return
	}

	targets := s.databases(r.Context())
	var target *backupTarget
	for _, t := range targets {
		if t.ID == dbName || t.Label == dbName || t.Name == dbName {
			target = &t
			break
		}
	}
	if target == nil {
		http.Error(w, fmt.Sprintf("database %q not found", dbName), http.StatusNotFound)
		return
	}

	if err := s.backupTarget(r.Context(), *target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": dbName})
}

func (s *Server) resolveRecord(dbName string) (*BackupRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[dbName]
	if ok {
		return rec, true
	}
	for _, r := range s.records {
		if r.DisplayName == dbName {
			return r, true
		}
	}
	return nil, false
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	dbName := r.PathValue("db")
	if dbName == "" {
		http.Error(w, "missing db name", http.StatusBadRequest)
		return
	}

	rec, ok := s.resolveRecord(dbName)
	if !ok || rec.LastBackupKey == "" {
		http.Error(w, fmt.Sprintf("no backup for %s", dbName), http.StatusNotFound)
		return
	}

	// If the backup is for a deleted app and >30 days old, prune instead of restore
	current := s.currentTargetKeys()
	if !current[rec.DBName] && rec.LastBackupAt != nil {
		maxAge := time.Duration(s.cfg.StaleBackupDays) * 24 * time.Hour
		if time.Since(*rec.LastBackupAt) > maxAge {
			log.Printf("restore refused: %s (%s) is stale (%d days), deleting", rec.DisplayName, rec.DBName, s.cfg.StaleBackupDays)
			for _, store := range s.stores {
				if err := store.DeletePrefix(r.Context(), rec.S3Prefix); err != nil {
					log.Printf("delete stale %s: %v", rec.DBName, err)
				}
			}
			s.db.Exec(r.Context(), "DELETE FROM backup_tracker WHERE db_name = $1", rec.DBName)
			s.mu.Lock()
			delete(s.records, rec.DBName)
			s.mu.Unlock()
			http.Error(w, fmt.Sprintf("backup for %s is over %d days old and has been deleted", dbName, s.cfg.StaleBackupDays), http.StatusNotFound)
			return
		}
	}

	store := s.restoreStore()
	rc, err := store.Load(r.Context(), rec.LastBackupKey)
	if err != nil {
		http.Error(w, fmt.Sprintf("load from %s: %v", store.Name(), err), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	gr, err := gzip.NewReader(rc)
	if err != nil {
		http.Error(w, fmt.Sprintf("gzip reader: %v", err), http.StatusInternalServerError)
		return
	}
	defer gr.Close()

	targets := s.databases(r.Context())
	var info *DBInfo
	for _, t := range targets {
		if t.ID == rec.DBName || t.Label == rec.DBName || t.Name == rec.DBName {
			info = &t.DBInfo
			break
		}
	}
	if info == nil {
		http.Error(w, "could not resolve db info for restore", http.StatusInternalServerError)
		return
	}

	dumper := s.dumpers.Get(info.Type)
	if dumper == nil {
		http.Error(w, fmt.Sprintf("no dumper for type %q", info.Type), http.StatusInternalServerError)
		return
	}

	if err := dumper.Restore(r.Context(), *info, gr, s.cfg.PostgresAdminPassword); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("restore: %s from %s/%s", dbName, store.Name(), rec.LastBackupKey)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": dbName, "restored_from": rec.LastBackupKey})
}

func (s *Server) restoreStore() Store {
	for _, store := range s.stores {
		if _, ok := store.(*S3Store); ok {
			return store
		}
	}
	return s.stores[0]
}
