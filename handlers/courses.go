package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"

	"readsync/db"
	"readsync/models"
)

func HandleGetCourses(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query("SELECT id, name, short_id, title_pattern, url_pattern, latest_url, latest_title, updated_at, created_at FROM courses ORDER BY created_at DESC")
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	courses := []models.Course{}
	for rows.Next() {
		var c models.Course
		if err := rows.Scan(&c.ID, &c.Name, &c.ShortID, &c.TitlePattern, &c.URLPattern, &c.LatestURL, &c.LatestTitle, &c.UpdatedAt, &c.CreatedAt); err != nil {
			continue
		}
		courses = append(courses, c)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(courses)
}

func HandleCreateCourse(w http.ResponseWriter, r *http.Request) {
	var c models.Course
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if c.Name == "" || c.ShortID == "" {
		http.Error(w, `{"error":"name and short_id are required"}`, http.StatusBadRequest)
		return
	}

	c.CreatedAt = time.Now().UTC().Format(time.RFC3339)

	result, err := db.DB.Exec(
		"INSERT INTO courses (name, short_id, title_pattern, url_pattern, latest_url, latest_title, updated_at, created_at) VALUES (?, ?, ?, ?, '', '', '', ?)",
		c.Name, c.ShortID, c.TitlePattern, c.URLPattern, c.CreatedAt,
	)
	if err != nil {
		http.Error(w, `{"error":"database error (short_id may already exist)"}`, http.StatusBadRequest)
		return
	}

	id, _ := result.LastInsertId()
	c.ID = id

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c)
}

func HandleUpdateCourse(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	var c models.Course
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	result, err := db.DB.Exec(
		"UPDATE courses SET name = ?, short_id = ?, title_pattern = ?, url_pattern = ? WHERE id = ?",
		c.Name, c.ShortID, c.TitlePattern, c.URLPattern, id,
	)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusBadRequest)
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	c.ID = id
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c)
}

func HandleDeleteCourse(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	result, err := db.DB.Exec("DELETE FROM courses WHERE id = ?", id)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

func HandleCourseJump(w http.ResponseWriter, r *http.Request, shortID string) {
	var latestURL, latestTitle string
	err := db.DB.QueryRow("SELECT latest_url, latest_title FROM courses WHERE short_id = ?", shortID).Scan(&latestURL, &latestTitle)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "course not found", http.StatusNotFound)
		} else {
			http.Error(w, "database error", http.StatusInternalServerError)
		}
		return
	}
	if latestURL == "" {
		http.Error(w, "course has no progress yet", http.StatusNotFound)
		return
	}

	if latestTitle == "" {
		latestTitle = latestURL
	}

	// Deliberately avoid http.Redirect / the Location header here. The upstream
	// IIS ARR reverse proxy has "reverse rewrite host in response headers"
	// enabled, which blindly replaces the host in Location with the proxy's own
	// host, corrupting external targets (e.g. cs61b-2.gitbook.io -> gmis.sdgh.net).
	// A body-based redirect is not rewritten by ARR.
	urlAttr := html.EscapeString(latestURL)
	urlJS, _ := json.Marshal(latestURL)
	titleText := html.EscapeString(latestTitle)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="0; url=%s">
<title>%s</title>
<script>location.replace(%s);</script>
</head>
<body>正在跳转到 <a href="%s">%s</a></body>
</html>`, urlAttr, titleText, string(urlJS), urlAttr, titleText)
}
