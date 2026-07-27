package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"readsync/db"
)

func callEntry(h func(http.ResponseWriter, *http.Request), method, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/entry", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func entryCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&n); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return n
}

func maxID(t *testing.T) int64 {
	t.Helper()
	var id sql.NullInt64
	if err := db.DB.QueryRow("SELECT MAX(id) FROM entries").Scan(&id); err != nil {
		t.Fatalf("max id: %v", err)
	}
	return id.Int64
}

// A title change for the URL of the latest entry updates that entry in place
// (no duplicate row) while advancing max(id) so the Web UI's /latest-id poll
// refreshes; a title change for any other URL is stored as a new entry.
func TestHandlePatchEntryTitle(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	h := &EntryHandlers{DedupeMinutes: 10, MaxEntries: 100000}

	if rec := callEntry(h.HandlePostEntry, http.MethodPost, `{"url":"https://x/a","title":"old"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed post: got %d, body %s", rec.Code, rec.Body.String())
	}
	seedID := maxID(t)

	// Same URL as the latest entry -> update in place, no new row...
	if rec := callEntry(h.HandlePatchEntryTitle, http.MethodPatch, `{"url":"https://x/a","title":"new"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch same url: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	if n := entryCount(t); n != 1 {
		t.Fatalf("expected 1 entry after in-place update, got %d", n)
	}
	var title string
	if err := db.DB.QueryRow("SELECT title FROM entries WHERE url = ?", "https://x/a").Scan(&title); err != nil {
		t.Fatalf("query title: %v", err)
	}
	if title != "new" {
		t.Fatalf("expected title updated to 'new', got %q", title)
	}
	// ...but max(id)/latest-id must advance so pollers notice the change.
	if got := maxID(t); got <= seedID {
		t.Fatalf("expected max(id) to advance past %d after title update, got %d", seedID, got)
	}
	if got := db.LatestEntryID.Load(); got != maxID(t) {
		t.Fatalf("LatestEntryID (%d) should track max(id) (%d)", got, maxID(t))
	}

	// Different URL from the latest entry -> insert a new entry.
	if rec := callEntry(h.HandlePatchEntryTitle, http.MethodPatch, `{"url":"https://x/b","title":"B title"}`); rec.Code != http.StatusCreated {
		t.Fatalf("patch new url: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	if n := entryCount(t); n != 2 {
		t.Fatalf("expected 2 entries after insert, got %d", n)
	}

	// Repeated title change for the latest URL updates in place, even though all
	// entries so far share the same created_at second (latest is resolved by id,
	// not created_at). Regression guard for a nondeterministic-ordering bug.
	beforeID := maxID(t)
	if rec := callEntry(h.HandlePatchEntryTitle, http.MethodPatch, `{"url":"https://x/b","title":"B2"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch latest url again: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	if n := entryCount(t); n != 2 {
		t.Fatalf("expected still 2 entries after in-place update, got %d", n)
	}
	if got := maxID(t); got <= beforeID {
		t.Fatalf("expected max(id) to advance past %d, got %d", beforeID, got)
	}

	// A missing URL is rejected.
	if rec := callEntry(h.HandlePatchEntryTitle, http.MethodPatch, `{"title":"no url"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch without url: expected 400, got %d", rec.Code)
	}
}

// An in-place title update keeps the entry's original created_at (so it stays in
// its place in the history) even though it is re-recorded under a fresh id.
func TestHandlePatchEntryTitlePreservesCreatedAt(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	h := &EntryHandlers{DedupeMinutes: 10, MaxEntries: 100000}

	const old = "2000-01-01T00:00:00Z"
	if _, err := db.DB.Exec("INSERT INTO entries (url, title, created_at) VALUES (?, ?, ?)", "https://x/a", "old", old); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.UpdateLatestEntryID()
	seedMax := maxID(t)

	if rec := callEntry(h.HandlePatchEntryTitle, http.MethodPatch, `{"url":"https://x/a","title":"new"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d, body %s", rec.Code, rec.Body.String())
	}

	var gotTitle, gotCreated string
	if err := db.DB.QueryRow("SELECT title, created_at FROM entries WHERE url = ?", "https://x/a").Scan(&gotTitle, &gotCreated); err != nil {
		t.Fatalf("query: %v", err)
	}
	if gotTitle != "new" {
		t.Fatalf("title: want %q, got %q", "new", gotTitle)
	}
	if gotCreated != old {
		t.Fatalf("created_at should be preserved as %q, got %q", old, gotCreated)
	}
	if got := maxID(t); got <= seedMax {
		t.Fatalf("max(id) should advance from %d, got %d", seedMax, got)
	}
	if n := entryCount(t); n != 1 {
		t.Fatalf("want 1 entry, got %d", n)
	}
}
