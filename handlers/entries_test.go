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

	h := &EntryHandlers{DedupeMinutes: 10, MaxEntries: 100000, HotEntries: 100000}

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

// 1-2 字符的短查询无法使用 FTS5 trigram 索引，应退化为 LIKE 子串匹配
// 而不是报 400；同时 LIKE 通配符应被转义为字面量。
func TestHandleGetEntriesShortSearchUsesLike(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	seed := []struct{ url, title, createdAt string }{
		{"https://example.com/ab/1", "first note", "2026-07-27T14:00:01Z"},
		{"https://example.com/other/2", "second note", "2026-07-27T14:00:02Z"},
		{"https://example.com/third", "ab note", "2026-07-27T14:00:03Z"},
		{"https://example.com/percent/4", "100% complete", "2026-07-27T14:00:04Z"},
	}
	for _, entry := range seed {
		if _, err := db.DB.Exec("INSERT INTO entries (url, title, created_at) VALUES (?, ?, ?)",
			entry.url, entry.title, entry.createdAt); err != nil {
			t.Fatalf("seed entry %q: %v", entry.url, err)
		}
	}

	// q=ab 同时命中 entry1 的 url 与 entry3 的 title，但不命中 entry2。
	request := httptest.NewRequest(http.MethodGet, "/api/v1/entry?q=ab", nil)
	recorder := httptest.NewRecorder()
	HandleGetEntries(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d, body %s", recorder.Code, recorder.Body.String())
	}

	var page struct {
		Entries []models.Entry `json:"entries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("expected 2 matches for q=ab, got %d: %#v", len(page.Entries), page.Entries)
	}
	// 按 created_at DESC 排序：entry3 在前，entry1 在后。
	if page.Entries[0].Title != "ab note" || page.Entries[1].URL != "https://example.com/ab/1" {
		t.Fatalf("unexpected matches for q=ab: %#v", page.Entries)
	}

	// 通配符 % 应转义为字面量：100% complete 中只有 % 与 % 字面匹配。
	request = httptest.NewRequest(http.MethodGet, "/api/v1/entry?q=100%25", nil)
	recorder = httptest.NewRecorder()
	HandleGetEntries(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("percent query: expected status 200, got %d, body %s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode percent response: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Title != "100% complete" {
		t.Fatalf("expected only the literal-%% match, got %#v", page.Entries)
	}
}

// An in-place title update keeps the entry's original created_at (so it stays in
// its place in the history) even though it is re-recorded under a fresh id.
func TestHandlePatchEntryTitlePreservesCreatedAt(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	h := &EntryHandlers{DedupeMinutes: 10, MaxEntries: 100000, HotEntries: 100000}

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

// 热表超过 HotEntries 后，最旧的条目自动搬入慢表，热表只保留最近 N 条。
func TestHandlePostEntryMigratesToSlowTable(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	h := &EntryHandlers{DedupeMinutes: 10, MaxEntries: 100000, HotEntries: 3}

	for i := 1; i <= 6; i++ {
		rec := callEntry(h.HandlePostEntry, http.MethodPost,
			fmt.Sprintf(`{"url":"https://example.com/%d","title":"entry %d"}`, i, i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %d: got %d, body %s", i, rec.Code, rec.Body.String())
		}
	}

	var hotCount, slowCount int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&hotCount); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM slow_entries").Scan(&slowCount); err != nil {
		t.Fatal(err)
	}
	if hotCount != 3 || slowCount != 3 {
		t.Fatalf("expected hot=3 slow=3, got hot=%d slow=%d", hotCount, slowCount)
	}

	// 慢表持有最旧的 3 条（id 1-3），热表持有最新的 3 条（id 4-6）。
	var slowMinID, hotMinID int64
	if err := db.DB.QueryRow("SELECT MIN(id) FROM slow_entries").Scan(&slowMinID); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT MIN(id) FROM entries").Scan(&hotMinID); err != nil {
		t.Fatal(err)
	}
	if slowMinID != 1 || hotMinID != 4 {
		t.Fatalf("expected slow starts at id 1 and hot at id 4, got slow=%d hot=%d", slowMinID, hotMinID)
	}
}

// 全局（热表+慢表）超过 MaxEntries 时，从慢表删除最旧的多余条目。
func TestHandlePostEntryEvictsSlowWhenOverGlobalCap(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	h := &EntryHandlers{DedupeMinutes: 10, MaxEntries: 5, HotEntries: 3}

	for i := 1; i <= 8; i++ {
		rec := callEntry(h.HandlePostEntry, http.MethodPost,
			fmt.Sprintf(`{"url":"https://example.com/%d","title":"entry %d"}`, i, i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %d: got %d, body %s", i, rec.Code, rec.Body.String())
		}
	}

	var hotCount, slowCount int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&hotCount); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM slow_entries").Scan(&slowCount); err != nil {
		t.Fatal(err)
	}
	// 批量搬移（batch = min(100, HotEntries) = 3）导致热表波动：最终 hot=2、slow=3，
	// 全局总数恒不超过 MaxEntries=5。
	if hotCount != 2 || slowCount != 3 {
		t.Fatalf("expected hot=2 slow=3, got hot=%d slow=%d", hotCount, slowCount)
	}

	// 慢表保留下限内的最旧条目：id 1-3 被逐步删除，保留 id 4、5、6。
	var slowMinID int64
	if err := db.DB.QueryRow("SELECT MIN(id) FROM slow_entries").Scan(&slowMinID); err != nil {
		t.Fatal(err)
	}
	if slowMinID != 4 {
		t.Fatalf("expected slow table to keep id 4 as its oldest, got %d", slowMinID)
	}
}

// 超限时一次性批量搬移（至少 hotMigrationBatch 条），而不是每超一条搬一条：
// HotEntries=100 时插入第 101 条触发一次搬移，应一次搬走 100 条而非 1 条。
func TestHandlePostEntryMigratesInBatches(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	h := &EntryHandlers{DedupeMinutes: 10, MaxEntries: 100000, HotEntries: 100}

	for i := 1; i <= 101; i++ {
		rec := callEntry(h.HandlePostEntry, http.MethodPost,
			fmt.Sprintf(`{"url":"https://example.com/%d","title":"entry %d"}`, i, i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %d: got %d, body %s", i, rec.Code, rec.Body.String())
		}
	}

	var hotCount, slowCount int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&hotCount); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM slow_entries").Scan(&slowCount); err != nil {
		t.Fatal(err)
	}
	// 插入 101 条时只触发一次搬移，一次搬 100 条：热表剩 1 条、慢表 100 条。
	if hotCount != 1 || slowCount != 100 {
		t.Fatalf("expected hot=1 slow=100 after a single batch migration, got hot=%d slow=%d", hotCount, slowCount)
	}
}

// 全局搜索跨热表与慢表检索，按 created_at DESC、来源（热表优先）、id DESC 排序；
// 支持 FTS（≥3 字符）、短查询 LIKE（1-2 字符）与跨表游标分页。
func TestHandleSearchEntriesAcrossTables(t *testing.T) {
	db.Init(filepath.Join(t.TempDir(), "data.db"), 100000)
	defer db.DB.Close()

	// 直接向两表播种（绕过迁移逻辑，便于精确控制分布）。
	seedHot := []struct{ url, title, createdAt string }{
		{"https://example.com/hot/a", "Alpha article", "2026-07-27T10:00:01Z"},
		{"https://example.com/hot/b", "Beta article", "2026-07-27T10:00:02Z"},
	}
	seedSlow := []struct{ url, title, createdAt string }{
		{"https://example.com/slow/a", "Alpha legacy note", "2026-07-27T09:00:01Z"},
		{"https://example.com/slow/b", "unrelated note", "2026-07-27T09:00:02Z"},
	}
	for _, entry := range seedHot {
		if _, err := db.DB.Exec("INSERT INTO entries (url, title, created_at) VALUES (?, ?, ?)",
			entry.url, entry.title, entry.createdAt); err != nil {
			t.Fatalf("seed hot %q: %v", entry.url, err)
		}
	}
	for _, entry := range seedSlow {
		if _, err := db.DB.Exec("INSERT INTO slow_entries (url, title, created_at) VALUES (?, ?, ?)",
			entry.url, entry.title, entry.createdAt); err != nil {
			t.Fatalf("seed slow %q: %v", entry.url, err)
		}
	}

	// "alpha" 同时命中热表与慢表，FTS 路径。
	request := httptest.NewRequest(http.MethodGet, "/api/v1/entry/search?q=alpha", nil)
	recorder := httptest.NewRecorder()
	HandleSearchEntries(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("search: got %d, body %s", recorder.Code, recorder.Body.String())
	}

	var page struct {
		Entries []models.Entry `json:"entries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode search response: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("expected 2 matches (hot + slow), got %d: %#v", len(page.Entries), page.Entries)
	}
	if page.Entries[0].Title != "Alpha article" || page.Entries[0].Source != "hot" {
		t.Fatalf("first match should be the hot Alpha article, got %#v", page.Entries[0])
	}
	if page.Entries[1].Title != "Alpha legacy note" || page.Entries[1].Source != "slow" {
		t.Fatalf("second match should be the slow Alpha note, got %#v", page.Entries[1])
	}

	// 短查询（1-2 字符）同样跨表命中。
	request = httptest.NewRequest(http.MethodGet, "/api/v1/entry/search?q=al", nil)
	recorder = httptest.NewRecorder()
	HandleSearchEntries(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("short search: got %d, body %s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode short search response: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("expected 2 matches for 'al', got %d: %#v", len(page.Entries), page.Entries)
	}

	// 分页：per_page=1 时第一页为热表 Alpha，游标翻页到慢表 Alpha。
	request = httptest.NewRequest(http.MethodGet, "/api/v1/entry/search?q=alpha&per_page=1", nil)
	recorder = httptest.NewRecorder()
	HandleSearchEntries(recorder, request)
	var firstPage struct {
		Entries    []models.Entry `json:"entries"`
		HasMore    bool           `json:"has_more"`
		NextCursor struct {
			CreatedAt string `json:"created_at"`
			Source    string `json:"source"`
			ID        int64  `json:"id"`
		} `json:"next_cursor"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &firstPage); err != nil {
		t.Fatalf("decode paged response: %v", err)
	}
	if len(firstPage.Entries) != 1 || !firstPage.HasMore {
		t.Fatalf("expected 1 entry with more pages, got %#v", firstPage)
	}
	if firstPage.Entries[0].Source != "hot" || firstPage.NextCursor.Source != "hot" {
		t.Fatalf("expected first page to end at the hot entry, got %#v", firstPage.NextCursor)
	}

	secondURL := "/api/v1/entry/search?q=alpha&per_page=1&cursor_created_at=" +
		url.QueryEscape(firstPage.NextCursor.CreatedAt) +
		"&cursor_id=" + strconv.FormatInt(firstPage.NextCursor.ID, 10) +
		"&cursor_source=" + firstPage.NextCursor.Source
	request = httptest.NewRequest(http.MethodGet, secondURL, nil)
	recorder = httptest.NewRecorder()
	HandleSearchEntries(recorder, request)
	var secondPage struct {
		Entries []models.Entry `json:"entries"`
		HasMore bool           `json:"has_more"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &secondPage); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if len(secondPage.Entries) != 1 || secondPage.HasMore {
		t.Fatalf("expected final single slow entry, got %#v", secondPage)
	}
	if secondPage.Entries[0].Source != "slow" || secondPage.Entries[0].Title != "Alpha legacy note" {
		t.Fatalf("second page should be the slow Alpha note, got %#v", secondPage.Entries[0])
	}
}
