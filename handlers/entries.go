package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"readsync/db"
	"readsync/models"
)

func AuthMiddleware(next http.HandlerFunc, username, password string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

type EntryHandlers struct {
	DedupeMinutes int
	MaxEntries    int
}

func (h *EntryHandlers) HandlePostEntry(w http.ResponseWriter, r *http.Request) {
	var entry models.Entry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if entry.URL == "" {
		http.Error(w, `{"error":"url is required"}`, http.StatusBadRequest)
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	var lastEntry models.Entry
	var lastCreatedAt string
	// Latest entry by insertion order. Use id, not created_at: created_at has
	// second precision, so entries created in the same second tie and ordering by
	// created_at would pick a nondeterministic row.
	err = tx.QueryRow("SELECT id, url, title, created_at FROM entries ORDER BY id DESC LIMIT 1").Scan(&lastEntry.ID, &lastEntry.URL, &lastEntry.Title, &lastCreatedAt)
	if err == nil && lastEntry.URL == entry.URL {
		t, parseErr := time.Parse(time.RFC3339, lastCreatedAt)
		if parseErr == nil && time.Since(t) < time.Duration(h.DedupeMinutes)*time.Minute {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(lastEntry)
			return
		}
	}

	if err := h.insertEntryTx(tx, &entry); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	db.LatestEntryID.Store(entry.ID)
	go db.MatchAndStoreCourse(entry.URL, entry.Title)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(entry)
}

// insertEntryTx evicts the oldest entries when the table is at capacity, then
// inserts entry within tx, stamping its CreatedAt and ID. It does not commit.
func (h *EntryHandlers) insertEntryTx(tx *sql.Tx, entry *models.Entry) error {
	var count int
	tx.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count)
	if count >= h.MaxEntries {
		deleteCount := int(float64(h.MaxEntries) * 0.05)
		if deleteCount < 1 {
			deleteCount = 1
		}
		if _, err := tx.Exec(
			"DELETE FROM entries WHERE id IN (SELECT id FROM entries ORDER BY created_at ASC LIMIT ?)",
			deleteCount,
		); err != nil {
			return err
		}
	}

	entry.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	result, err := tx.Exec(
		"INSERT INTO entries (url, title, created_at) VALUES (?, ?, ?)",
		entry.URL, entry.Title, entry.CreatedAt,
	)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	entry.ID = id
	return nil
}

// HandlePatchEntryTitle records a title change for the current page. If the most
// recent entry is for the same URL, its title is updated in place; otherwise the
// change is stored as a new entry. The userscript calls this when document.title
// changes without a navigation.
func (h *EntryHandlers) HandlePatchEntryTitle(w http.ResponseWriter, r *http.Request) {
	var entry models.Entry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if entry.URL == "" {
		http.Error(w, `{"error":"url is required"}`, http.StatusBadRequest)
		return
	}

	tx, err := db.DB.Begin()
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	var lastEntry models.Entry
	// Latest entry by insertion order (id, not created_at — see HandlePostEntry).
	err = tx.QueryRow("SELECT id, url, title, created_at FROM entries ORDER BY id DESC LIMIT 1").Scan(&lastEntry.ID, &lastEntry.URL, &lastEntry.Title, &lastEntry.CreatedAt)
	replaced := err == nil && lastEntry.URL == entry.URL
	if replaced {
		// Same page, title changed. Re-insert under a fresh id instead of a bare
		// UPDATE so max(id) — and therefore /latest-id — advances, which lets the
		// Web UI's polling show the new title live. Preserve created_at so the
		// entry keeps its place in the history rather than jumping to the top.
		if _, err := tx.Exec("DELETE FROM entries WHERE id = ?", lastEntry.ID); err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		entry.CreatedAt = lastEntry.CreatedAt
		result, err := tx.Exec(
			"INSERT INTO entries (url, title, created_at) VALUES (?, ?, ?)",
			entry.URL, entry.Title, entry.CreatedAt,
		)
		if err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		id, _ := result.LastInsertId()
		entry.ID = id
	} else {
		// The changed page is no longer the latest entry (or there are none yet):
		// store the title change as a fresh entry.
		if err := h.insertEntryTx(tx, &entry); err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	db.LatestEntryID.Store(entry.ID)
	go db.MatchAndStoreCourse(entry.URL, entry.Title)

	w.Header().Set("Content-Type", "application/json")
	if !replaced {
		w.WriteHeader(http.StatusCreated)
	}
	json.NewEncoder(w).Encode(entry)
}

func HandleGetEntries(w http.ResponseWriter, r *http.Request) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if search != "" && utf8.RuneCountInString(search) < 3 {
		http.Error(w, `{"error":"search query must contain at least 3 characters"}`, http.StatusBadRequest)
		return
	}
	cursorCreatedAt := strings.TrimSpace(r.URL.Query().Get("cursor_created_at"))
	cursorID, cursorError := strconv.ParseInt(r.URL.Query().Get("cursor_id"), 10, 64)
	hasCursor := cursorCreatedAt != "" || r.URL.Query().Get("cursor_id") != ""
	if hasCursor && (cursorCreatedAt == "" || cursorError != nil || cursorID < 1) {
		http.Error(w, `{"error":"invalid cursor"}`, http.StatusBadRequest)
		return
	}

	limit := perPage + 1
	queryArguments := []interface{}{}
	var queryBuilder strings.Builder
	queryBuilder.WriteString("SELECT entries.id, entries.url, entries.title, entries.created_at FROM entries")

	conditions := []string{}
	if search != "" {
		queryBuilder.WriteString(" JOIN entries_fts ON entries_fts.rowid = entries.id")
		conditions = append(conditions, "entries_fts MATCH ?")
		queryArguments = append(queryArguments, buildFTSQuery(search))
	}
	if hasCursor {
		conditions = append(conditions, "(entries.created_at < ? OR (entries.created_at = ? AND entries.id < ?))")
		queryArguments = append(queryArguments, cursorCreatedAt, cursorCreatedAt, cursorID)
	}
	if len(conditions) > 0 {
		queryBuilder.WriteString(" WHERE ")
		queryBuilder.WriteString(strings.Join(conditions, " AND "))
	}
	queryBuilder.WriteString(" ORDER BY entries.created_at DESC, entries.id DESC LIMIT ?")
	queryArguments = append(queryArguments, limit)

	rows, err := db.DB.QueryContext(r.Context(), queryBuilder.String(), queryArguments...)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	entries := []models.Entry{}
	for rows.Next() {
		var entry models.Entry
		if err := rows.Scan(&entry.ID, &entry.URL, &entry.Title, &entry.CreatedAt); err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	hasMore := len(entries) > perPage
	if hasMore {
		entries = entries[:perPage]
	}

	var nextCursor interface{}
	if hasMore && len(entries) > 0 {
		lastEntry := entries[len(entries)-1]
		nextCursor = map[string]interface{}{
			"created_at": lastEntry.CreatedAt,
			"id":         lastEntry.ID,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries":     entries,
		"per_page":    perPage,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}

func buildFTSQuery(search string) string {
	terms := strings.Fields(search)
	quotedTerms := make([]string, 0, len(terms))
	for _, term := range terms {
		escapedTerm := strings.ReplaceAll(term, `"`, `""`)
		quotedTerms = append(quotedTerms, `"`+escapedTerm+`"*`)
	}
	return strings.Join(quotedTerms, " AND ")
}

func HandleDeleteEntry(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	result, err := db.DB.Exec("DELETE FROM entries WHERE id = ?", id)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	db.UpdateLatestEntryID()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

func HandleGetLatestID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"latest_id": db.LatestEntryID.Load()})
}
