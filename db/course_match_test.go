package db

import (
	"path/filepath"
	"testing"
	"time"
)

// Regression test: MatchAndStoreCourse must update a course's latest_url when an
// entry matches its url_pattern. Previously the UPDATE ran while the SELECT rows
// cursor was still open, which returned SQLITE_BUSY and silently dropped the write.
func TestMatchAndStoreCourse(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "data.db")
	Init(tmp, 100000)
	defer DB.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := DB.Exec(
		"INSERT INTO courses (name, short_id, title_pattern, url_pattern, latest_url, latest_title, updated_at, created_at) VALUES (?, ?, ?, ?, '', '', '', ?)",
		"CS61B", "cs61b", "", `https://cs61b-2.gitbook.io/.+`, now,
	)
	if err != nil {
		t.Fatalf("insert course: %v", err)
	}

	url := "https://cs61b-2.gitbook.io/cs61b-textbook-2025/17.-b-trees/17.3-b-tree-operations"
	title := "17.3 B-Tree Operations | CS61B Textbook 2025"

	MatchAndStoreCourse(url, title)

	var latestURL, latestTitle string
	if err := DB.QueryRow("SELECT latest_url, latest_title FROM courses WHERE short_id = ?", "cs61b").Scan(&latestURL, &latestTitle); err != nil {
		t.Fatalf("query course: %v", err)
	}
	if latestURL != url {
		t.Fatalf("expected latest_url=%q, got %q", url, latestURL)
	}
	if latestTitle != title {
		t.Fatalf("expected latest_title=%q, got %q", title, latestTitle)
	}
}
