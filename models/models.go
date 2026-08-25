package models

import "time"

type Entry struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	// Source 标记条目来自热表还是慢表，仅全局搜索响应中填充。
	Source string `json:"source,omitempty"`
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
	Username string `json:"username"`
	Password string `json:"password"`
	Port     int    `json:"port"`
	DBPath   string `json:"db_path"`
	BaseURL  string `json:"base_url"`
	// Host 部署时必须显式指定的对外访问地址（可含 scheme 与端口，如 "https://read.example.com:8443"）。
	// 它是用户脚本中服务端地址与 @connect 主机的唯一来源，不做自动推断。
	Host       string `json:"host"`
	MaxEntries int    `json:"max_entries"`
	// HotEntries 热表保留的最近条目数，超出部分自动搬移到慢表，默认 2000。
	HotEntries     int `json:"hot_entries"`
	DedupeMinutes  int `json:"dedupe_minutes"`
	PollIntervalMs int `json:"poll_interval_ms"`
}

func NewEntry(url, title string) Entry {
	return Entry{
		URL:       url,
		Title:     title,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}
