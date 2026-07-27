package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"readsync/db"
	"readsync/models"
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

func TestHandleGetEntriesUsesFTSAndCursorPagination(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	for entryIndex := 1; entryIndex <= 5; entryIndex++ {
		createdAt := fmt.Sprintf("2026-07-27T12:00:%02dZ", entryIndex)
		title := fmt.Sprintf("SQLite search result %d", entryIndex)
		if entryIndex == 3 {
			title = "unrelated entry"
		}
		if _, err := db.DB.Exec(
			"INSERT INTO entries (url, title, created_at) VALUES (?, ?, ?)",
			fmt.Sprintf("https://example.com/articles/%d", entryIndex), title, createdAt,
		); err != nil {
			t.Fatalf("seed entry %d: %v", entryIndex, err)
		}
	}

	firstRequest := httptest.NewRequest(http.MethodGet, "/api/v1/entry?per_page=2&q=search", nil)
	firstRecorder := httptest.NewRecorder()
	HandleGetEntries(firstRecorder, firstRequest)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first page: got %d, body %s", firstRecorder.Code, firstRecorder.Body.String())
	}

	var firstPage struct {
		Entries    []models.Entry `json:"entries"`
		HasMore    bool           `json:"has_more"`
		NextCursor struct {
			CreatedAt string `json:"created_at"`
			ID        int64  `json:"id"`
		} `json:"next_cursor"`
	}
	if err := json.Unmarshal(firstRecorder.Body.Bytes(), &firstPage); err != nil {
		t.Fatalf("decode first page: %v", err)
	}
	if len(firstPage.Entries) != 2 || !firstPage.HasMore {
		t.Fatalf("first page: expected 2 entries and more results, got %d and has_more=%v", len(firstPage.Entries), firstPage.HasMore)
	}
	if firstPage.Entries[0].Title != "SQLite search result 5" || firstPage.Entries[1].Title != "SQLite search result 4" {
		t.Fatalf("first page returned unexpected titles: %#v", firstPage.Entries)
	}

	secondURL := "/api/v1/entry?per_page=2&q=search&cursor_created_at=" + url.QueryEscape(firstPage.NextCursor.CreatedAt) +
		"&cursor_id=" + strconv.FormatInt(firstPage.NextCursor.ID, 10)
	secondRequest := httptest.NewRequest(http.MethodGet, secondURL, nil)
	secondRecorder := httptest.NewRecorder()
	HandleGetEntries(secondRecorder, secondRequest)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second page: got %d, body %s", secondRecorder.Code, secondRecorder.Body.String())
	}

	var secondPage struct {
		Entries []models.Entry `json:"entries"`
		HasMore bool           `json:"has_more"`
	}
	if err := json.Unmarshal(secondRecorder.Body.Bytes(), &secondPage); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if len(secondPage.Entries) != 2 || secondPage.HasMore {
		t.Fatalf("second page: expected final 2 entries, got %d and has_more=%v", len(secondPage.Entries), secondPage.HasMore)
	}
	if secondPage.Entries[0].Title != "SQLite search result 2" || secondPage.Entries[1].Title != "SQLite search result 1" {
		t.Fatalf("second page returned unexpected titles: %#v", secondPage.Entries)
	}
}

func TestHandleGetEntriesRejectsShortSearch(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/entry?q=ab", nil)
	recorder := httptest.NewRecorder()
	HandleGetEntries(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d, body %s", recorder.Code, recorder.Body.String())
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
