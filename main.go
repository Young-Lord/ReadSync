package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed webui/*
var webuiFS embed.FS

type Config struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	Port           int    `json:"port"`
	DBPath         string `json:"db_path"`
	BaseURL        string `json:"base_url"`
	MaxEntries     int    `json:"max_entries"`
	DedupeMinutes  int    `json:"dedupe_minutes"`
	PollIntervalMs int    `json:"poll_interval_ms"`
}

type Entry struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
}

var db *sql.DB
var config Config
var latestEntryID atomic.Int64

func loadConfig(path string) Config {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Failed to parse config: %v", err)
	}
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "data.db"
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 100000
	}
	if cfg.DedupeMinutes <= 0 {
		cfg.DedupeMinutes = 10
	}
	if cfg.PollIntervalMs <= 0 {
		cfg.PollIntervalMs = 30000
	}
	return cfg
}

func initDB(path string) {
	var err error
	db, err = sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}

	query := `CREATE TABLE IF NOT EXISTS entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`
	if _, err := db.Exec(query); err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}

	query = `CREATE INDEX IF NOT EXISTS idx_entries_created_at ON entries(created_at DESC)`
	if _, err := db.Exec(query); err != nil {
		log.Fatalf("Failed to create index: %v", err)
	}

	var maxID sql.NullInt64
	if err := db.QueryRow("SELECT MAX(id) FROM entries").Scan(&maxID); err != nil {
		log.Fatalf("Failed to query latest entry ID: %v", err)
	}
	if maxID.Valid {
		latestEntryID.Store(maxID.Int64)
	}
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != config.Username || pass != config.Password {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func handlePostEntry(w http.ResponseWriter, r *http.Request) {
	var entry Entry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if entry.URL == "" {
		http.Error(w, `{"error":"url is required"}`, http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// Deduplication: if the latest entry has the same URL and was created recently, skip insert
	var lastEntry Entry
	var lastCreatedAt string
	err = tx.QueryRow("SELECT id, url, title, created_at FROM entries ORDER BY created_at DESC LIMIT 1").Scan(&lastEntry.ID, &lastEntry.URL, &lastEntry.Title, &lastCreatedAt)
	if err == nil && lastEntry.URL == entry.URL {
		t, parseErr := time.Parse(time.RFC3339, lastCreatedAt)
		if parseErr == nil && time.Since(t) < time.Duration(config.DedupeMinutes)*time.Minute {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(lastEntry)
			return
		}
	}

	// Eviction: if entries exceed max, delete the oldest 5%
	var count int
	tx.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count)
	if count >= config.MaxEntries {
		deleteCount := int(math.Max(1, float64(config.MaxEntries)*0.05))
		if _, err := tx.Exec(
			"DELETE FROM entries WHERE id IN (SELECT id FROM entries ORDER BY created_at ASC LIMIT ?)",
			deleteCount,
		); err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
	}

	entry.CreatedAt = time.Now().UTC().Format(time.RFC3339)

	result, err := tx.Exec(
		"INSERT INTO entries (url, title, created_at) VALUES (?, ?, ?)",
		entry.URL, entry.Title, entry.CreatedAt,
	)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	id, _ := result.LastInsertId()
	entry.ID = id
	latestEntryID.Store(id)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(entry)
}

func handleGetEntries(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))

	var rows *sql.Rows
	var err error
	offset := (page - 1) * perPage
	limit := perPage + 1

	if search != "" {
		like := "%" + search + "%"
		rows, err = db.Query(
			`SELECT id, url, title, created_at FROM entries
			 WHERE url LIKE ? OR title LIKE ?
			 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
			like, like, limit, offset,
		)
	} else {
		rows, err = db.Query(
			`SELECT id, url, title, created_at FROM entries
			 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
			limit, offset,
		)
	}
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	entries := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.URL, &e.Title, &e.CreatedAt); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	hasMore := len(entries) > perPage
	if hasMore {
		entries = entries[:perPage]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries":  entries,
		"page":     page,
		"per_page": perPage,
		"has_more": hasMore,
	})
}

func handleDeleteEntry(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	result, err := db.Exec("DELETE FROM entries WHERE id = ?", id)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	var maxID sql.NullInt64
	if err := db.QueryRow("SELECT MAX(id) FROM entries").Scan(&maxID); err == nil {
		if maxID.Valid {
			latestEntryID.Store(maxID.Int64)
		} else {
			latestEntryID.Store(0)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

func handleGetLatestID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"latest_id": latestEntryID.Load()})
}

func makeHandleAPI(apiPrefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, apiPrefix)
		path = strings.TrimPrefix(path, "/")

		if r.Method == http.MethodPost && path == "" {
			handlePostEntry(w, r)
			return
		}
		if r.Method == http.MethodGet && path == "" {
			handleGetEntries(w, r)
			return
		}
		if r.Method == http.MethodGet && path == "latest-id" {
			handleGetLatestID(w, r)
			return
		}
		if r.Method == http.MethodDelete && path != "" {
			handleDeleteEntry(w, r, path)
			return
		}

		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func main() {
	config = loadConfig("config.json")
	initDB(config.DBPath)
	defer db.Close()

	base := strings.TrimSuffix(config.BaseURL, "/")

	mux := http.NewServeMux()

	apiPrefix := base + "/api/v1/entry"
	mux.HandleFunc(apiPrefix, authMiddleware(makeHandleAPI(apiPrefix)))
	mux.HandleFunc(apiPrefix+"/", authMiddleware(makeHandleAPI(apiPrefix)))

	webPrefix := base + "/"
	webFS, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		log.Fatalf("Failed to get webui subfilesystem: %v", err)
	}
	fsrv := http.FileServer(http.FS(webFS))
	mux.HandleFunc(webPrefix, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == webPrefix || r.URL.Path == base || r.URL.Path == base+"/" {
			html, err := webuiFS.ReadFile("webui/index.html")
			if err != nil {
				log.Printf("Failed to read embedded index.html: %v", err)
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			content := strings.Replace(string(html), "<!--BASE_URL-->", base, 1)
			content = strings.Replace(content, "<!--POLL_INTERVAL-->", strconv.Itoa(config.PollIntervalMs), 1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(content))
			return
		}
		if base != "" {
			http.StripPrefix(base, fsrv).ServeHTTP(w, r)
		} else {
			fsrv.ServeHTTP(w, r)
		}
	})

	addr := fmt.Sprintf(":%d", config.Port)
	log.Printf("ReadSync server starting on %s", addr)
	if base == "" {
		log.Printf("Web UI: http://localhost%s/", addr)
		log.Printf("API:    http://localhost%s/api/v1/entry", addr)
	} else {
		log.Printf("Web UI: http://localhost%s%s/", addr, base)
		log.Printf("API:    http://localhost%s%s/api/v1/entry", addr, base)
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
