package db

import (
	"database/sql"
	"log"
	"math"
	"regexp"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var DB *sql.DB
var LatestEntryID atomic.Int64

func Init(path string, maxEntries int) {
	var err error
	DB, err = sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
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
	if _, err := DB.Exec("CREATE INDEX IF NOT EXISTS idx_entries_created_at ON entries(created_at DESC)"); err != nil {
		log.Fatalf("Failed to create entries index: %v", err)
	}

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

func EvictOldEntries(maxEntries int) {
	var count int
	DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count)
	if count < maxEntries {
		return
	}
	deleteCount := int(math.Max(1, float64(maxEntries)*0.05))
	DB.Exec("DELETE FROM entries WHERE id IN (SELECT id FROM entries ORDER BY created_at ASC LIMIT ?)", deleteCount)
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
		return
	}
	defer rows.Close()

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
			DB.Exec(
				"UPDATE courses SET latest_url = ?, latest_title = ?, updated_at = ? WHERE id = ?",
				entryURL, entryTitle, now, courseID,
			)
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
