package db

import (
	"database/sql"
	"log"
	"regexp"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var DB *sql.DB
var LatestEntryID atomic.Int64

func Init(path string, maxEntries int) {
	var err error
	// modernc.org/sqlite ignores mattn-style DSN params (_journal_mode/_busy_timeout),
	// so use its _pragma form; otherwise WAL and the busy timeout never take effect
	// and concurrent writes fail with SQLITE_BUSY.
	DB, err = sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}

	entriesDDL := `CREATE TABLE IF NOT EXISTS entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`
	if _, err := DB.Exec(entriesDDL); err != nil {
		log.Fatalf("Failed to create entries table: %v", err)
	}
	if _, err := DB.Exec("CREATE INDEX IF NOT EXISTS idx_entries_created_at_id ON entries(created_at DESC, id DESC)"); err != nil {
		log.Fatalf("Failed to create entries index: %v", err)
	}
	initializeEntriesSearchIndex()

	// 慢表：最近 HotEntries 条之外的数据自动搬移到这里，保持热表轻量。
	// id 不做自增，直接沿用热表搬移过来的原 id。
	slowEntriesDDL := `CREATE TABLE IF NOT EXISTS slow_entries (
		id INTEGER PRIMARY KEY,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`
	if _, err := DB.Exec(slowEntriesDDL); err != nil {
		log.Fatalf("Failed to create slow_entries table: %v", err)
	}
	if _, err := DB.Exec("CREATE INDEX IF NOT EXISTS idx_slow_entries_created_at_id ON slow_entries(created_at DESC, id DESC)"); err != nil {
		log.Fatalf("Failed to create slow entries index: %v", err)
	}
	initializeSlowEntriesSearchIndex()

	coursesDDL := `CREATE TABLE IF NOT EXISTS courses (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		short_id TEXT NOT NULL UNIQUE,
		title_pattern TEXT NOT NULL DEFAULT '',
		url_pattern TEXT NOT NULL DEFAULT '',
		latest_url TEXT NOT NULL DEFAULT '',
		latest_title TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`
	if _, err := DB.Exec(coursesDDL); err != nil {
		log.Fatalf("Failed to create courses table: %v", err)
	}
	if _, err := DB.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_courses_short_id ON courses(short_id)"); err != nil {
		log.Fatalf("Failed to create courses index: %v", err)
	}

	var maxID sql.NullInt64
	if err := DB.QueryRow("SELECT MAX(id) FROM entries").Scan(&maxID); err != nil {
		log.Fatalf("Failed to query latest entry ID: %v", err)
	}
	if maxID.Valid {
		LatestEntryID.Store(maxID.Int64)
	}

	_ = maxEntries
}

func initializeEntriesSearchIndex() {
	var searchTableExists bool
	if err := DB.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'entries_fts')",
	).Scan(&searchTableExists); err != nil {
		log.Fatalf("Failed to inspect entries search index: %v", err)
	}

	statements := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
			title,
			url,
			content='entries',
			content_rowid='id',
			tokenize='trigram case_sensitive 0'
		)`,
		`CREATE TRIGGER IF NOT EXISTS entries_fts_after_insert AFTER INSERT ON entries BEGIN
			INSERT INTO entries_fts(rowid, title, url) VALUES (new.id, new.title, new.url);
		END`,
		`CREATE TRIGGER IF NOT EXISTS entries_fts_after_delete AFTER DELETE ON entries BEGIN
			INSERT INTO entries_fts(entries_fts, rowid, title, url) VALUES ('delete', old.id, old.title, old.url);
		END`,
		`CREATE TRIGGER IF NOT EXISTS entries_fts_after_update AFTER UPDATE ON entries BEGIN
			INSERT INTO entries_fts(entries_fts, rowid, title, url) VALUES ('delete', old.id, old.title, old.url);
			INSERT INTO entries_fts(rowid, title, url) VALUES (new.id, new.title, new.url);
		END`,
	}
	for _, statement := range statements {
		if _, err := DB.Exec(statement); err != nil {
			log.Fatalf("Failed to initialize entries search index: %v", err)
		}
	}

	if !searchTableExists {
		if _, err := DB.Exec("INSERT INTO entries_fts(entries_fts) VALUES ('rebuild')"); err != nil {
			log.Fatalf("Failed to rebuild entries search index: %v", err)
		}
	}
}

func initializeSlowEntriesSearchIndex() {
	var searchTableExists bool
	if err := DB.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'slow_entries_fts')",
	).Scan(&searchTableExists); err != nil {
		log.Fatalf("Failed to inspect slow entries search index: %v", err)
	}

	statements := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS slow_entries_fts USING fts5(
			title,
			url,
			content='slow_entries',
			content_rowid='id',
			tokenize='trigram case_sensitive 0'
		)`,
		`CREATE TRIGGER IF NOT EXISTS slow_entries_fts_after_insert AFTER INSERT ON slow_entries BEGIN
			INSERT INTO slow_entries_fts(rowid, title, url) VALUES (new.id, new.title, new.url);
		END`,
		`CREATE TRIGGER IF NOT EXISTS slow_entries_fts_after_delete AFTER DELETE ON slow_entries BEGIN
			INSERT INTO slow_entries_fts(slow_entries_fts, rowid, title, url) VALUES ('delete', old.id, old.title, old.url);
		END`,
		`CREATE TRIGGER IF NOT EXISTS slow_entries_fts_after_update AFTER UPDATE ON slow_entries BEGIN
			INSERT INTO slow_entries_fts(slow_entries_fts, rowid, title, url) VALUES ('delete', old.id, old.title, old.url);
			INSERT INTO slow_entries_fts(rowid, title, url) VALUES (new.id, new.title, new.url);
		END`,
	}
	for _, statement := range statements {
		if _, err := DB.Exec(statement); err != nil {
			log.Fatalf("Failed to initialize slow entries search index: %v", err)
		}
	}

	if !searchTableExists {
		if _, err := DB.Exec("INSERT INTO slow_entries_fts(slow_entries_fts) VALUES ('rebuild')"); err != nil {
			log.Fatalf("Failed to rebuild slow entries search index: %v", err)
		}
	}
}

func UpdateLatestEntryID() {
	var maxID sql.NullInt64
	if err := DB.QueryRow("SELECT MAX(id) FROM entries").Scan(&maxID); err == nil {
		if maxID.Valid {
			LatestEntryID.Store(maxID.Int64)
		} else {
			LatestEntryID.Store(0)
		}
	}
}

func MatchAndStoreCourse(entryURL, entryTitle string) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := DB.Query("SELECT id, title_pattern, url_pattern FROM courses")
	if err != nil {
		log.Printf("MatchAndStoreCourse: query courses failed: %v", err)
		return
	}

	var matchedIDs []int64
	for rows.Next() {
		var courseID int64
		var titlePat, urlPat string
		if err := rows.Scan(&courseID, &titlePat, &urlPat); err != nil {
			continue
		}

		matched := false
		if urlPat != "" {
			if matchRegex(urlPat, entryURL) {
				matched = true
			}
		}
		if !matched && titlePat != "" {
			if matchRegex(titlePat, entryTitle) {
				matched = true
			}
		}
		if matched {
			matchedIDs = append(matchedIDs, courseID)
		}
	}
	// Close the read cursor before issuing writes: SQLite returns SQLITE_BUSY
	// if we UPDATE while this query's rows are still open.
	rows.Close()

	for _, courseID := range matchedIDs {
		if _, err := DB.Exec(
			"UPDATE courses SET latest_url = ?, latest_title = ?, updated_at = ? WHERE id = ?",
			entryURL, entryTitle, now, courseID,
		); err != nil {
			log.Printf("MatchAndStoreCourse: update course %d failed: %v", courseID, err)
		}
	}
}

func matchRegex(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}
