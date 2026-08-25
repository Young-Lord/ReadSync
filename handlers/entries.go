package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
	HotEntries    int
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

// insertEntryTx inserts entry within tx, stamping its CreatedAt and ID, then
// migrates overflow into the slow table and evicts from it if the global cap is
// exceeded. It does not commit.
func (h *EntryHandlers) insertEntryTx(tx *sql.Tx, entry *models.Entry) error {
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
	return h.migrateToSlowAndEvict(tx)
}

// hotMigrationBatch 热表超限触发搬移时的一次最少搬移条数：批量搬移让热表
// 留出缓冲余量，避免"每插一条就搬一条"导致的频繁写放大。
const hotMigrationBatch = 100

// migrateToSlowAndEvict 保持热表不超过 HotEntries 条：超限时一次性把最旧的
// 多余条目批量搬进慢表（至少 hotMigrationBatch 条，不超过热表上限，防止小
// 配置把热表搬空）；若全局（热表+慢表）超过 MaxEntries，再从慢表删除最旧的
// 多余条目。搬移复用两表的 FTS 触发器，两边的全文索引自动保持同步。
func (h *EntryHandlers) migrateToSlowAndEvict(tx *sql.Tx) error {
	var hotCount int
	if err := tx.QueryRow("SELECT COUNT(*) FROM entries").Scan(&hotCount); err != nil {
		return err
	}
	if hotCount > h.HotEntries {
		batch := hotMigrationBatch
		if batch > h.HotEntries {
			batch = h.HotEntries
		}
		migrateCount := hotCount - h.HotEntries
		if migrateCount < batch {
			migrateCount = batch
		}
		if _, err := tx.Exec(
			"INSERT INTO slow_entries (id, url, title, created_at) "+
				"SELECT id, url, title, created_at FROM entries "+
				"ORDER BY created_at ASC, id ASC LIMIT ?", migrateCount,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			"DELETE FROM entries WHERE id IN ("+
				"SELECT id FROM entries ORDER BY created_at ASC, id ASC LIMIT ?)", migrateCount,
		); err != nil {
			return err
		}
	}

	var totalCount int
	if err := tx.QueryRow(
		"SELECT (SELECT COUNT(*) FROM entries) + (SELECT COUNT(*) FROM slow_entries)",
	).Scan(&totalCount); err != nil {
		return err
	}
	if totalCount > h.MaxEntries {
		excess := totalCount - h.MaxEntries
		if _, err := tx.Exec(
			"DELETE FROM slow_entries WHERE id IN ("+
				"SELECT id FROM slow_entries ORDER BY created_at ASC, id ASC LIMIT ?)", excess,
		); err != nil {
			return err
		}
	}
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
	cursorCreatedAt := strings.TrimSpace(r.URL.Query().Get("cursor_created_at"))
	cursorID, cursorError := strconv.ParseInt(r.URL.Query().Get("cursor_id"), 10, 64)
	hasCursor := cursorCreatedAt != "" || r.URL.Query().Get("cursor_id") != ""
	if hasCursor && (cursorCreatedAt == "" || cursorError != nil || cursorID < 1) {
		http.Error(w, `{"error":"invalid cursor"}`, http.StatusBadRequest)
		return
	}

	// 1-2 字符的查询无法走 FTS5 trigram 索引，退化为全表 LIKE 扫描；
	// 首页结果用短 TTL 缓存避免用户在输入过程中反复触发扫描。
	isShortSearch := search != "" && utf8.RuneCountInString(search) < 3
	if isShortSearch && !hasCursor {
		if cached := lookupShortQueryCache(db.DB, search, perPage); cached != nil {
			writeEntriesResponse(w, perPage, cached.entries, cached.hasMore, cached.nextCursor)
			return
		}
	}

	limit := perPage + 1
	queryArguments := []interface{}{}
	var queryBuilder strings.Builder
	queryBuilder.WriteString("SELECT entries.id, entries.url, entries.title, entries.created_at FROM entries")

	conditions := []string{}
	if search != "" {
		if utf8.RuneCountInString(search) >= 3 {
			queryBuilder.WriteString(" JOIN entries_fts ON entries_fts.rowid = entries.id")
			conditions = append(conditions, "entries_fts MATCH ?")
			queryArguments = append(queryArguments, buildFTSQuery(search))
		} else {
			// trigram 分词器要求每个查询词至少 3 个字符，更短的查询只能用 LIKE
			// 子串匹配。SQLite 的 LIKE 对 ASCII 大小写不敏感，与 FTS5
			// case_sensitive 0 的行为一致。
			escapedSearch := escapeLikePattern(search)
			pattern := "%" + escapedSearch + "%"
			conditions = append(conditions, "(entries.title LIKE ? ESCAPE '\\' OR entries.url LIKE ? ESCAPE '\\')")
			queryArguments = append(queryArguments, pattern, pattern)
		}
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

	if isShortSearch && !hasCursor {
		storeShortQueryCache(db.DB, search, perPage, entries, hasMore, nextCursor)
	}

	writeEntriesResponse(w, perPage, entries, hasMore, nextCursor)
}

// HandleSearchEntries 全局搜索：跨热表与慢表检索，按 created_at DESC、
// 来源（热表优先）、id DESC 统一排序。游标格式 {created_at, source, id}。
// 与普通列表一致，≥3 字符走各表 FTS 索引，1-2 字符退化为 LIKE 扫描并缓存首页。
func HandleSearchEntries(w http.ResponseWriter, r *http.Request) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if search == "" {
		http.Error(w, `{"error":"q is required"}`, http.StatusBadRequest)
		return
	}

	cursorCreatedAt := strings.TrimSpace(r.URL.Query().Get("cursor_created_at"))
	cursorID, cursorError := strconv.ParseInt(r.URL.Query().Get("cursor_id"), 10, 64)
	cursorSource := strings.TrimSpace(r.URL.Query().Get("cursor_source"))
	hasCursor := cursorCreatedAt != "" || r.URL.Query().Get("cursor_id") != "" || cursorSource != ""
	cursorRank := 0
	if hasCursor {
		if cursorCreatedAt == "" || cursorError != nil || cursorID < 1 {
			http.Error(w, `{"error":"invalid cursor"}`, http.StatusBadRequest)
			return
		}
		switch cursorSource {
		case "", "hot":
			cursorRank = 0
		case "slow":
			cursorRank = 1
		default:
			http.Error(w, `{"error":"invalid cursor"}`, http.StatusBadRequest)
			return
		}
	}

	isShortSearch := utf8.RuneCountInString(search) < 3
	limit := perPage + 1

	// 用 "global:" 前缀与普通列表的短查询缓存区分开。
	if isShortSearch && !hasCursor {
		if cached := lookupShortQueryCache(db.DB, "global:"+search, perPage); cached != nil {
			writeEntriesResponse(w, perPage, cached.entries, cached.hasMore, cached.nextCursor)
			return
		}
	}

	tables := []struct {
		table string // entries / slow_entries
		fts   string // entries_fts / slow_entries_fts
		rank  int    // 0 = 热表优先
	}{
		{table: "entries", fts: "entries_fts", rank: 0},
		{table: "slow_entries", fts: "slow_entries_fts", rank: 1},
	}

	queryArguments := []interface{}{}
	var queryBuilder strings.Builder
	queryBuilder.WriteString("SELECT * FROM (")
	for index, table := range tables {
		if index > 0 {
			queryBuilder.WriteString(" UNION ALL ")
		}
		queryBuilder.WriteString(fmt.Sprintf(
			"SELECT %[1]s.id AS id, %[1]s.url AS url, %[1]s.title AS title, %[1]s.created_at AS created_at, %[2]d AS source_rank FROM %[1]s",
			table.table, table.rank,
		))

		conditions := []string{}
		if isShortSearch {
			pattern := "%" + escapeLikePattern(search) + "%"
			conditions = append(conditions, fmt.Sprintf(
				"(%s.title LIKE ? ESCAPE '\\' OR %s.url LIKE ? ESCAPE '\\')",
				table.table, table.table,
			))
			queryArguments = append(queryArguments, pattern, pattern)
		} else {
			queryBuilder.WriteString(fmt.Sprintf(" JOIN %s ON %s.rowid = %s.id", table.fts, table.fts, table.table))
			conditions = append(conditions, table.fts+" MATCH ?")
			queryArguments = append(queryArguments, buildFTSQuery(search))
		}

		if hasCursor {
			// 全局排序为 (created_at DESC, source_rank ASC, id DESC)，游标按同一顺序回退。
			conditions = append(conditions, fmt.Sprintf(
				"(%s.created_at < ? OR (%s.created_at = ? AND (? < ? OR (? = ? AND %s.id < ?))))",
				table.table, table.table, table.table,
			))
			queryArguments = append(queryArguments,
				cursorCreatedAt, cursorCreatedAt, cursorRank, table.rank, cursorRank, table.rank, cursorID,
			)
		}
		if len(conditions) > 0 {
			queryBuilder.WriteString(" WHERE ")
			queryBuilder.WriteString(strings.Join(conditions, " AND "))
		}
	}
	queryBuilder.WriteString(") ORDER BY created_at DESC, source_rank ASC, id DESC LIMIT ?")
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
		var sourceRank int
		if err := rows.Scan(&entry.ID, &entry.URL, &entry.Title, &entry.CreatedAt, &sourceRank); err != nil {
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		if sourceRank == 1 {
			entry.Source = "slow"
		} else {
			entry.Source = "hot"
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
			"source":     lastEntry.Source,
			"id":         lastEntry.ID,
		}
	}

	if isShortSearch && !hasCursor {
		storeShortQueryCache(db.DB, "global:"+search, perPage, entries, hasMore, nextCursor)
	}

	writeEntriesResponse(w, perPage, entries, hasMore, nextCursor)
}

func writeEntriesResponse(w http.ResponseWriter, perPage int, entries []models.Entry, hasMore bool, nextCursor interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries":     entries,
		"per_page":    perPage,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}

// 短查询缓存：1-2 字符的搜索在输入过程中会高频触发，而它只能全表扫描，
// 因此对首页结果做短 TTL 缓存。key 携带 *sql.DB 以保证不同数据库实例（测试）隔离。
var shortQueryCache sync.Map // key: shortQueryCacheKey -> *shortQueryCacheEntry

const shortQueryCacheTTL = 5 * time.Second

type shortQueryCacheKey struct {
	db      *sql.DB
	query   string
	perPage int
}

type shortQueryCacheEntry struct {
	entries    []models.Entry
	hasMore    bool
	nextCursor interface{}
	expiresAt  time.Time
}

func lookupShortQueryCache(dbHandle *sql.DB, query string, perPage int) *shortQueryCacheEntry {
	key := shortQueryCacheKey{db: dbHandle, query: query, perPage: perPage}
	if value, ok := shortQueryCache.Load(key); ok {
		entry := value.(*shortQueryCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry
		}
		shortQueryCache.Delete(key)
	}
	return nil
}

func storeShortQueryCache(dbHandle *sql.DB, query string, perPage int, entries []models.Entry, hasMore bool, nextCursor interface{}) {
	key := shortQueryCacheKey{db: dbHandle, query: query, perPage: perPage}
	shortQueryCache.Store(key, &shortQueryCacheEntry{
		entries:    entries,
		hasMore:    hasMore,
		nextCursor: nextCursor,
		expiresAt:  time.Now().Add(shortQueryCacheTTL),
	})
}

// escapeLikePattern 转义 LIKE 模式中的通配符，使用户输入按字面量匹配。
func escapeLikePattern(search string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search)
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
