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
	"os/exec"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"
)

var identRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type Config struct {
	DatabaseURL            string
	RegistryURL            string
	BackupInterval         time.Duration
	S3Bucket               string
	AWSRegion              string
	KeycloakDBPassword     string
	RegistryDBPassword     string
	PostgresAdminPassword  string
}

type DBApp struct {
	Name          string `json:"name"`
	ContainerName string `json:"container_name"`
	Metadata      struct {
		DBName string `json:"db_name"`
		DBUser string `json:"db_user"`
	} `json:"metadata"`
}

type BackupRecord struct {
	DBName       string     `json:"db_name"`
	DBUser       string     `json:"db_user"`
	LastBackupAt *time.Time `json:"last_backup_at"`
	LastBackupKey string   `json:"last_backup_key"`
	BackupCount  int        `json:"backup_count"`
	S3Prefix     string     `json:"s3_prefix"`
}

type Server struct {
	cfg         Config
	pool        *pgxpool.Pool
	s3Client    *s3.Client
	mu          sync.RWMutex
	records     map[string]*BackupRecord
	// password for hardcoded DBs by db_name
	hardcodedPasswords map[string]string
}

type hardcodedDB struct {
	name     string
	user     string
	password string
}

func main() {
	cfg := loadConfig()

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS backup_tracker (
		db_name TEXT PRIMARY KEY,
		db_user TEXT NOT NULL,
		last_backup_at TIMESTAMPTZ,
		last_backup_key TEXT,
		backup_count INT DEFAULT 0,
		s3_prefix TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		log.Fatalf("Failed to create backup_tracker table: %v", err)
	}

	s3cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}
	s3Client := s3.NewFromConfig(s3cfg)

	s := &Server{
		cfg:      cfg,
		pool:     pool,
		s3Client: s3Client,
		records:  make(map[string]*BackupRecord),
		hardcodedPasswords: map[string]string{
			"keycloak": cfg.KeycloakDBPassword,
			"registry": cfg.RegistryDBPassword,
		},
	}

	s.loadRecords(ctx)

	go s.scheduleBackups(ctx)

	go func() {
		time.Sleep(5 * time.Second)
		log.Println("Running initial backup sweep")
		s.runAllBackups(ctx)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/backups", s.handleListBackups)
	mux.HandleFunc("/api/backups/{db}/backup", s.handleTriggerBackup)
	mux.HandleFunc("/api/backups/{db}/restore", s.handleRestore)

	addr := ":7273"
	log.Printf("Backup service listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
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
		AWSRegion:             getEnv("AWS_REGION", "us-east-1"),
		KeycloakDBPassword:    os.Getenv("KEYCLOAK_DB_PASSWORD"),
		RegistryDBPassword:    os.Getenv("REGISTRY_DB_PASSWORD"),
		PostgresAdminPassword: os.Getenv("POSTGRES_ADMIN_PASSWORD"),
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// databases returns the deduplicated list of databases to back up.
func (s *Server) databases(ctx context.Context) []DBApp {
	seen := make(map[string]bool)

	hardcoded := []hardcodedDB{
		{name: "keycloak", user: "keycloak", password: s.hardcodedPasswords["keycloak"]},
		{name: "registry", user: "registry", password: s.hardcodedPasswords["registry"]},
	}

	var apps []DBApp
	for _, h := range hardcoded {
		if seen[h.name] {
			continue
		}
		seen[h.name] = true
		apps = append(apps, DBApp{
			Name: h.name,
			Metadata: struct {
				DBName string `json:"db_name"`
				DBUser string `json:"db_user"`
			}{DBName: h.name, DBUser: h.user},
		})
	}

	discovered, err := s.discoverDBs(ctx)
	if err != nil {
		log.Printf("Warning: failed to discover DBs from registry: %v", err)
	} else {
		for _, d := range discovered {
			if seen[d.Name] {
				continue
			}
			seen[d.Name] = true
			apps = append(apps, d)
		}
	}
	return apps
}

func (s *Server) discoverDBs(ctx context.Context) ([]DBApp, error) {
	url := fmt.Sprintf("%s/api/apps?type=db", s.cfg.RegistryURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}

	var apps []DBApp
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// dbPassword returns the password for the connecting user.
// Hardcoded DBs use their own user; discovered DBs connect as admin.
func (s *Server) dbPassword(db DBApp) string {
	if pwd, ok := s.hardcodedPasswords[db.Name]; ok && pwd != "" {
		return pwd
	}
	return s.cfg.PostgresAdminPassword
}

// dbUser returns the postgres user to connect as.
// Hardcoded DBs use the application user; discovered DBs connect as admin.
func (s *Server) dbUser(db DBApp) string {
	if _, ok := s.hardcodedPasswords[db.Name]; ok {
		if db.Metadata.DBUser != "" {
			return db.Metadata.DBUser
		}
		return db.Name
	}
	return "postgres"
}

func (s *Server) loadRecords(ctx context.Context) {
	rows, err := s.pool.Query(ctx, "SELECT db_name, db_user, last_backup_at, last_backup_key, backup_count, s3_prefix FROM backup_tracker")
	if err != nil {
		log.Printf("Warning: failed to load backup records: %v", err)
		return
	}
	defer rows.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	for rows.Next() {
		var r BackupRecord
		if err := rows.Scan(&r.DBName, &r.DBUser, &r.LastBackupAt, &r.LastBackupKey, &r.BackupCount, &r.S3Prefix); err != nil {
			log.Printf("Warning: failed to scan backup record: %v", err)
			continue
		}
		s.records[r.DBName] = &r
	}
}

func (s *Server) upsertRecord(ctx context.Context, r *BackupRecord) error {
	s.mu.Lock()
	s.records[r.DBName] = r
	s.mu.Unlock()

	_, err := s.pool.Exec(ctx, `INSERT INTO backup_tracker (db_name, db_user, last_backup_at, last_backup_key, backup_count, s3_prefix)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (db_name) DO UPDATE SET
			db_user = EXCLUDED.db_user,
			last_backup_at = EXCLUDED.last_backup_at,
			last_backup_key = EXCLUDED.last_backup_key,
			backup_count = EXCLUDED.backup_count,
			s3_prefix = EXCLUDED.s3_prefix`,
		r.DBName, r.DBUser, r.LastBackupAt, r.LastBackupKey, r.BackupCount, r.S3Prefix)
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
			log.Println("Running scheduled backup sweep")
			s.runAllBackups(ctx)
		}
	}
}

func (s *Server) runAllBackups(ctx context.Context) {
	dbs := s.databases(ctx)
	for _, db := range dbs {
		if err := s.backupDatabase(ctx, db); err != nil {
			log.Printf("Backup failed for %s: %v", db.Name, err)
		}
	}
}

func (s *Server) backupDatabase(ctx context.Context, db DBApp) error {
	dbName := db.Name
	user := s.dbUser(db)
	password := s.dbPassword(db)
	if password == "" {
		return fmt.Errorf("no password available for %s", dbName)
	}

	if s.cfg.S3Bucket == "" {
		return fmt.Errorf("BACKUP_S3_BUCKET not configured")
	}

	timestamp := time.Now().UTC().Format("20060102T150405Z")
	key := fmt.Sprintf("backups/%s/%s.sql.gz", dbName, timestamp)

	// pg_dump -> compress -> upload to S3
	var dumpBuf bytes.Buffer
	pgdump := exec.Command("pg_dump", "-U", user, "-d", dbName, "-h", "postgres")
	pgdump.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", password))
	pgdump.Stdout = &dumpBuf
	pgdump.Stderr = os.Stderr

	if err := pgdump.Run(); err != nil {
		return fmt.Errorf("pg_dump failed: %w", err)
	}

	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write(dumpBuf.Bytes()); err != nil {
		return fmt.Errorf("gzip write: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("gzip close: %w", err)
	}

	_, err := s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.S3Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(compressed.Bytes()),
	})
	if err != nil {
		return fmt.Errorf("S3 upload failed: %w", err)
	}

	now := time.Now()
	prefix := fmt.Sprintf("backups/%s", dbName)
	rec := &BackupRecord{
		DBName:       dbName,
		DBUser:       user,
		LastBackupAt: &now,
		LastBackupKey: key,
		S3Prefix:     prefix,
	}

	// Load existing record to get current count
	s.mu.RLock()
	existing, exists := s.records[dbName]
	s.mu.RUnlock()
	if exists {
		rec.BackupCount = existing.BackupCount + 1
	} else {
		rec.BackupCount = 1
	}

	if err := s.upsertRecord(ctx, rec); err != nil {
		log.Printf("Warning: failed to update backup tracker for %s: %v", dbName, err)
	}

	log.Printf("Backup complete: %s -> s3://%s/%s", dbName, s.cfg.S3Bucket, key)
	return nil
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
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

	dbs := s.databases(r.Context())
	var target *DBApp
	for _, db := range dbs {
		if db.Name == dbName {
			target = &db
			break
		}
	}
	if target == nil {
		http.Error(w, fmt.Sprintf("database %q not found", dbName), http.StatusNotFound)
		return
	}

	if err := s.backupDatabase(r.Context(), *target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": dbName})
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	dbName := r.PathValue("db")
	if dbName == "" {
		http.Error(w, "missing db name", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	rec, ok := s.records[dbName]
	s.mu.RUnlock()
	if !ok || rec.LastBackupKey == "" {
		http.Error(w, fmt.Sprintf("no backup found for %s", dbName), http.StatusNotFound)
		return
	}

	// Download from S3
	result, err := s.s3Client.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.S3Bucket),
		Key:    aws.String(rec.LastBackupKey),
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get backup from S3: %v", err), http.StatusInternalServerError)
		return
	}
	defer result.Body.Close()

	gr, err := gzip.NewReader(result.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create gzip reader: %v", err), http.StatusInternalServerError)
		return
	}
	defer gr.Close()

	if !identRe.MatchString(dbName) || !identRe.MatchString(rec.DBUser) {
		http.Error(w, "invalid database or user name", http.StatusBadRequest)
		return
	}

	// Drop and recreate database as admin
	adminPassword := s.cfg.PostgresAdminPassword
	if adminPassword == "" {
		http.Error(w, "POSTGRES_ADMIN_PASSWORD not configured", http.StatusInternalServerError)
		return
	}

	adminEnv := append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", adminPassword))

	dropCreate := exec.Command("psql", "-U", "postgres", "-h", "postgres",
		"-c", `DROP DATABASE IF EXISTS "`+dbName+`"`,
		"-c", `CREATE DATABASE "`+dbName+`" OWNER "`+rec.DBUser+`"`)
	dropCreate.Env = adminEnv
	dropCreate.Stderr = os.Stderr
	if out, err := dropCreate.Output(); err != nil {
		http.Error(w, fmt.Sprintf("failed to drop/recreate database: %v\n%s", err, string(out)), http.StatusInternalServerError)
		return
	}

	// Restore from decompressed backup
	restore := exec.Command("psql", "-U", rec.DBUser, "-d", dbName, "-h", "postgres")
	restore.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", s.dbPassword(DBApp{
		Name: dbName,
		Metadata: struct {
			DBName string `json:"db_name"`
			DBUser string `json:"db_user"`
		}{DBName: dbName, DBUser: rec.DBUser},
	})))
	restore.Stdin = gr
	restore.Stderr = os.Stderr
	if out, err := restore.Output(); err != nil {
		http.Error(w, fmt.Sprintf("restore failed: %v\n%s", err, string(out)), http.StatusInternalServerError)
		return
	}

	log.Printf("Restore complete: %s from s3://%s/%s", dbName, s.cfg.S3Bucket, rec.LastBackupKey)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": dbName, "restored_from": rec.LastBackupKey})
}
