package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"readsync/db"
	"readsync/handlers"
	"readsync/models"
)

//go:embed webui/*
var webuiFS embed.FS

func loadConfig(path string) models.Config {
	var cfg models.Config
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Failed to parse config: %v", err)
	}
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "data.db"
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 100000
	}
	if cfg.DedupeMinutes <= 0 {
		cfg.DedupeMinutes = 10
	}
	if cfg.PollIntervalMs <= 0 {
		cfg.PollIntervalMs = 30000
	}
	return cfg
}

func makeEntryAPIHandler(apiPrefix string, entryH *handlers.EntryHandlers) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, apiPrefix)
		path = strings.TrimPrefix(path, "/")

		if r.Method == http.MethodPost && path == "" {
			entryH.HandlePostEntry(w, r)
			return
		}
		if r.Method == http.MethodGet && path == "" {
			handlers.HandleGetEntries(w, r)
			return
		}
		if r.Method == http.MethodGet && path == "latest-id" {
			handlers.HandleGetLatestID(w, r)
			return
		}
		if r.Method == http.MethodDelete && path != "" {
			handlers.HandleDeleteEntry(w, r, path)
			return
		}

		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func makeCourseAPIHandler(apiPrefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, apiPrefix)
		path = strings.TrimPrefix(path, "/")

		if r.Method == http.MethodGet && path == "" {
			handlers.HandleGetCourses(w, r)
			return
		}
		if r.Method == http.MethodPost && path == "" {
			handlers.HandleCreateCourse(w, r)
			return
		}
		if r.Method == http.MethodPut && path != "" {
			handlers.HandleUpdateCourse(w, r, path)
			return
		}
		if r.Method == http.MethodDelete && path != "" {
			handlers.HandleDeleteCourse(w, r, path)
			return
		}

		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func main() {
	config := loadConfig("config.json")
	db.Init(config.DBPath, config.MaxEntries)
	defer db.DB.Close()

	base := strings.TrimSuffix(config.BaseURL, "/")
	entryH := &handlers.EntryHandlers{
		DedupeMinutes: config.DedupeMinutes,
		MaxEntries:    config.MaxEntries,
	}

	mux := http.NewServeMux()

	apiPrefix := base + "/api/v1/entry"
	authEntry := handlers.AuthMiddleware(makeEntryAPIHandler(apiPrefix, entryH), config.Username, config.Password)
	mux.HandleFunc(apiPrefix, authEntry)
	mux.HandleFunc(apiPrefix+"/", authEntry)

	courseAPIPrefix := base + "/api/v1/course"
	authCourse := handlers.AuthMiddleware(makeCourseAPIHandler(courseAPIPrefix), config.Username, config.Password)
	mux.HandleFunc(courseAPIPrefix, authCourse)
	mux.HandleFunc(courseAPIPrefix+"/", authCourse)

	jumpPrefix := base + "/course/jump/"
	mux.HandleFunc(jumpPrefix, func(w http.ResponseWriter, r *http.Request) {
		shortID := strings.TrimPrefix(r.URL.Path, jumpPrefix)
		shortID = strings.TrimSuffix(shortID, "/")
		if shortID == "" {
			http.Error(w, "short_id required", http.StatusBadRequest)
			return
		}
		handlers.HandleCourseJump(w, r, shortID)
	})

	webPrefix := base + "/"
	webFS, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		log.Fatalf("Failed to get webui subfilesystem: %v", err)
	}
	fsrv := http.FileServer(http.FS(webFS))
	mux.HandleFunc(webPrefix, func(w http.ResponseWriter, r *http.Request) {
		rPath := r.URL.Path
		if base != "" {
			rPath = strings.TrimPrefix(rPath, base)
		}
		rPath = strings.TrimPrefix(rPath, "/")

		if rPath == "" || rPath == "index.html" {
			html, err := webuiFS.ReadFile("webui/index.html")
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			content := strings.Replace(string(html), "<!--BASE_URL-->", base, 1)
			content = strings.Replace(content, "<!--POLL_INTERVAL-->", strconv.Itoa(config.PollIntervalMs), 1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(content))
			return
		}

		if rPath == "courses.html" || rPath == "courses" {
			html, err := webuiFS.ReadFile("webui/courses.html")
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			content := strings.Replace(string(html), "<!--BASE_URL-->", base, 1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(content))
			return
		}

		if base != "" {
			http.StripPrefix(base, fsrv).ServeHTTP(w, r)
		} else {
			fsrv.ServeHTTP(w, r)
		}
	})

	addr := fmt.Sprintf(":%d", config.Port)
	log.Printf("ReadSync server starting on %s", addr)
	if base == "" {
		log.Printf("Web UI:  http://localhost%s/", addr)
		log.Printf("API:     http://localhost%s/api/v1/entry", addr)
		log.Printf("Courses: http://localhost%s/courses.html", addr)
		log.Printf("Jump:    http://localhost%s/course/jump/<short_id>", addr)
	} else {
		log.Printf("Web UI:  http://localhost%s%s/", addr, base)
		log.Printf("API:     http://localhost%s%s/api/v1/entry", addr, base)
		log.Printf("Courses: http://localhost%s%s/courses.html", addr, base)
		log.Printf("Jump:    http://localhost%s%s/course/jump/<short_id>", addr, base)
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
