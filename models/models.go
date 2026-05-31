package models

import "time"

type Entry struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
}

type Course struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	ShortID      string `json:"short_id"`
	TitlePattern string `json:"title_pattern"`
	URLPattern   string `json:"url_pattern"`
	LatestURL    string `json:"latest_url"`
	LatestTitle  string `json:"latest_title"`
	UpdatedAt    string `json:"updated_at"`
	CreatedAt    string `json:"created_at"`
}

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

func NewEntry(url, title string) Entry {
	return Entry{
		URL:       url,
		Title:     title,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}
