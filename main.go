package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/mark0725/go-agents-proxy/internal/api"
	"github.com/mark0725/go-agents-proxy/internal/config"
	"github.com/mark0725/go-agents-proxy/internal/logger"
	"github.com/mark0725/go-agents-proxy/internal/proxy"
)

//go:embed web/*
var webFS embed.FS

func main() {
	_ = godotenv.Load()

	cfgPath := "config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	// Load config
	cfgMgr, err := config.NewManager(cfgPath)
	if err != nil {
		slog.Error("failed to load config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Watch config for hot reload
	if err := cfgMgr.Watch(); err != nil {
		slog.Error("failed to watch config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Init logger
	logDir := "logs"
	_ = os.MkdirAll(logDir, 0755)
	log := logger.New(logDir)
	logLevel := cfgMgr.Get().App.Level
	if logLevel == "" {
		logLevel = os.Getenv("LOG_LEVEL")
	}
	if logLevel == "" {
		logLevel = "info"
	}
	if err := log.InitSlog(logLevel); err != nil {
		slog.Error("failed to init logger", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Handlers
	proxyHandler := proxy.NewHandler(cfgMgr, log)
	apiHandler := api.NewHandler(cfgMgr, log)

	mux := http.NewServeMux()

	// LLM routes
	mux.HandleFunc("/llm/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/count_tokens") {
			proxyHandler.HandleCountTokens(w, r)
		} else {
			proxyHandler.HandleMessages(w, r)
		}
	})

	// Management API
	apiHandler.RegisterRoutes(mux)

	// Static files / Admin UI
	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		slog.Error("failed to create sub fs", slog.String("error", err.Error()))
		os.Exit(1)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			data, err := webFS.ReadFile("web/index.html")
			if err != nil {
				http.Error(w, "Not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			w.Write(data)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	// Panic recovery middleware
	handler := recoverMiddleware(mux)

	port := cfgMgr.Get().App.Port
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "8082"
	}

	listen := cfgMgr.Get().App.Listen
	if listen == "" {
		listen = "0.0.0.0"
	}
	addr := listen + ":" + port

	slog.Info("server starting", slog.String("addr", addr))
	slog.Info("routes",
		slog.String("llm", "/llm/<route-id>/v1/messages"),
		slog.String("admin", "/"),
		slog.String("api", "/api/*"),
	)

	if err := http.ListenAndServe(addr, handler); err != nil {
		slog.Error("server failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("http panic recovered",
					slog.String("path", r.URL.Path),
					slog.String("method", r.Method),
					slog.Any("panic", rec),
				)
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{
						"type":    "internal_error",
						"message": "Internal server error",
					},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
