package main

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"readsync/db"
	"readsync/handlers"
	"readsync/models"
)

//go:embed webui/*
var webuiFS embed.FS

// installTokenStore 管理一次性安装令牌及其对应的预生成脚本内容。
// 前端 POST 换取令牌时，服务端已用用户的认证凭据生成了完整脚本。
type installTokenStore struct {
	mu     sync.RWMutex
	tokens map[string]string // token -> pre-generated script content
}

var installTokens = &installTokenStore{
	tokens: make(map[string]string),
}

const installTokenTTL = 5 * time.Minute

func (s *installTokenStore) generate(scriptContent string) string {
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[token] = scriptContent
	s.mu.Unlock()
	// 异步清理：5 分钟后自动过期
	time.AfterFunc(installTokenTTL, func() {
		s.mu.Lock()
		delete(s.tokens, token)
		s.mu.Unlock()
	})
	return token
}

// consume 查询令牌对应的预生成脚本内容。token 可多次使用，TTL 过期后由 AfterFunc 清理。
func (s *installTokenStore) consume(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	content, ok := s.tokens[token]
	return content, ok
}

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
	if cfg.Host == "" {
		log.Fatalf("Failed to load config: host is required. " +
			"Set \"host\" in config.json to the public address used by browsers (e.g. \"https://read.example.com\").")
	}
	return cfg
}

func makeEntryAPIHandler(apiPrefix string, entryH *handlers.EntryHandlers) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
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
		if r.Method == http.MethodPatch && path == "" {
			entryH.HandlePatchEntryTitle(w, r)
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

// resolvePublicEndpoint 计算用户脚本中的服务端地址（SERVER_URL）与 @connect 主机。
// host 的唯一来源是部署配置 config.Host（支持 "host"、"host:port"、"scheme://host:port" 等格式），
// 不做任何请求级自动推断。
func resolvePublicEndpoint(cfgHost, base string) (serverURL, connectHost string) {
	scheme := "http"
	host := cfgHost
	if idx := strings.Index(host, "://"); idx != -1 {
		scheme = host[:idx]
		host = host[idx+3:]
	}
	host = strings.Trim(host, "/")
	return fmt.Sprintf("%s://%s%s", scheme, host, strings.TrimSuffix(base, "/")), stripPort(host)
}

// stripPort 去掉 host 中的端口部分并移除 IPv6 括号，用于 @connect 指令。
func stripPort(host string) string {
	if colonIdx := strings.LastIndex(host, ":"); colonIdx != -1 {
		portStr := host[colonIdx+1:]
		if _, err := strconv.Atoi(portStr); err == nil {
			host = host[:colonIdx]
		}
	}
	return strings.Trim(host, "[]")
}

// generateUserscriptContent 读取模板并用用户凭据填充占位符，返回完整的 .user.js 内容。
func generateUserscriptContent(user, pass, base, cfgHost string) (string, error) {
	template, err := webuiFS.ReadFile("webui/userscript.template.js")
	if err != nil {
		return "", err
	}

	serverURL, connectHost := resolvePublicEndpoint(cfgHost, base)

	authToken := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))

	content := string(template)
	content = strings.ReplaceAll(content, "<!--CONNECT_HOST-->", connectHost)
	content = strings.ReplaceAll(content, "<!--SERVER_URL-->", serverURL)
	content = strings.ReplaceAll(content, "<!--AUTH_TOKEN-->", authToken)
	return content, nil
}

// makeUserscriptHandler 生成动态用户脚本。
// Basic Auth → 实时生成。?token=xxx → 从令牌存储中取出预生成内容。
func makeUserscriptHandler(base, cfgHost string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok {
			content, err := generateUserscriptContent(user, pass, base, cfgHost)
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write([]byte(content))
			return
		}

		// 令牌认证：取出预生成脚本
		token := r.URL.Query().Get("token")
		content, ok := installTokens.consume(token)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write([]byte(content))
	}
}

// makeInstallTokenHandler 处理 POST /token 请求。
// 已在 AuthMiddleware 中通过 Basic Auth 验证，此处预生成完整脚本并存入令牌存储。
func makeInstallTokenHandler(base, cfgHost string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		content, err := generateUserscriptContent(user, pass, base, cfgHost)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		token := installTokens.generate(content)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": token})
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

	// 用户脚本安装路由 — 无需 AuthMiddleware，处理器内自行判断 Basic Auth 或 ?token=
	userscriptPrefix := base + "/userscript.user.js"
	mux.HandleFunc(userscriptPrefix, makeUserscriptHandler(base, config.Host))

	// 安装令牌 API — AuthMiddleware 保护，只允许 POST
	tokenAPIPrefix := base + "/api/v1/userscript/token"
	authTokenAPI := handlers.AuthMiddleware(makeInstallTokenHandler(base, config.Host), config.Username, config.Password)
	mux.HandleFunc(tokenAPIPrefix, authTokenAPI)

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

		if rPath == "sw.js" {
			script, err := webuiFS.ReadFile("webui/sw.js")
			if err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Service-Worker-Allowed", webPrefix)
			w.Write(script)
			return
		}

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
		log.Printf("Userscript: http://localhost%s/userscript.user.js", addr)
		log.Printf("Jump:    http://localhost%s/course/jump/<short_id>", addr)
	} else {
		log.Printf("Web UI:  http://localhost%s%s/", addr, base)
		log.Printf("API:     http://localhost%s%s/api/v1/entry", addr, base)
		log.Printf("Courses: http://localhost%s%s/courses.html", addr, base)
		log.Printf("Userscript: http://localhost%s%s/userscript.user.js", addr, base)
		log.Printf("Jump:    http://localhost%s%s/course/jump/<short_id>", addr, base)
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
