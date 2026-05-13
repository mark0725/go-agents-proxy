package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mark0725/go-agents-proxy/internal/config"
	"github.com/mark0725/go-agents-proxy/internal/logger"
)

// Handler holds dependencies for management API endpoints.
type Handler struct {
	Config *config.Manager
	Logger *logger.Logger
}

// NewHandler creates a new API handler.
func NewHandler(cfg *config.Manager, log *logger.Logger) *Handler {
	return &Handler{Config: cfg, Logger: log}
}

func (h *Handler) validateAdminAPIKey(r *http.Request) bool {
	cfg := h.Config.Get()
	if !cfg.App.Auth {
		return true
	}
	apiKey := r.Header.Get("x-api-key")
	if apiKey == "" {
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			apiKey = strings.TrimPrefix(authHeader, "Bearer ")
		}
	}

	for _, u := range cfg.Users {
		if u.Token == apiKey || u.Password == apiKey {
			return true
		}
	}
	return false
}

func sendJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func sendError(w http.ResponseWriter, status int, message string) {
	slog.Error("api error", slog.Int("status", status), slog.String("message", message))
	sendJSON(w, status, map[string]interface{}{
		"error": map[string]string{
			"type":    "api_error",
			"message": message,
		},
	})
}

func sendValidationError(w http.ResponseWriter, errs []config.ValidationError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "validation_error",
			"message": "Invalid configuration",
			"details": errs,
		},
	})
}

// RegisterRoutes registers all /api/* handlers.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/config", h.handleConfig)
	mux.HandleFunc("/api/logs/llm", h.handleLLMLogs)
	mux.HandleFunc("/api/logs/app", h.handleAppLogs)
	mux.HandleFunc("/api/routes", h.handleRoutes)
	mux.HandleFunc("/api/providers", h.handleProviders)
}

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !h.validateAdminAPIKey(r) {
		sendError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}

	switch r.Method {
	case http.MethodGet:
		cfg := h.Config.Get()
		sendJSON(w, http.StatusOK, cfg)

	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			sendError(w, http.StatusBadRequest, "Failed to read body")
			return
		}
		var newCfg config.Config
		if err := json.Unmarshal(body, &newCfg); err != nil {
			sendError(w, http.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
			return
		}
		config.NormalizeConfig(&newCfg)
		if errs := config.ValidateConfig(&newCfg); len(errs) > 0 {
			sendValidationError(w, errs)
			return
		}
		if err := h.Config.SaveConfig(&newCfg); err != nil {
			sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to save config: %v", err))
			return
		}
		sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (h *Handler) handleLLMLogs(w http.ResponseWriter, r *http.Request) {
	if !h.validateAdminAPIKey(r) {
		sendError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}

	if r.Method != http.MethodGet {
		sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	sortField := normalizeLLMLogSort(r.URL.Query().Get("sort"))
	sortDesc := r.URL.Query().Get("order") != "asc"
	statusFilter := normalizeLLMLogStatus(r.URL.Query().Get("status"))

	result, err := h.Logger.ReadLLMLogs(logger.LLMLogQuery{
		Date:         date,
		Offset:       offset,
		Limit:        limit,
		SortField:    sortField,
		SortDesc:     sortDesc,
		StatusFilter: statusFilter,
	})
	if err != nil {
		sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to read logs: %v", err))
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"date":      date,
		"total":     result.Total,
		"offset":    offset,
		"limit":     limit,
		"sort":      sortField,
		"order":     queryOrder(sortDesc),
		"status":    statusFilter,
		"truncated": result.Truncated,
		"logs":      result.Records,
	})
}

func normalizeLLMLogSort(value string) string {
	switch value {
	case "status_code", "duration_ms", "input_tokens", "output_tokens", "timestamp":
		return value
	default:
		return "timestamp"
	}
}

func normalizeLLMLogStatus(value string) string {
	switch value {
	case "success", "error":
		return value
	default:
		return ""
	}
}

func queryOrder(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

func (h *Handler) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	if !h.validateAdminAPIKey(r) {
		sendError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}

	if r.Method != http.MethodGet {
		sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}

	lines, err := h.Logger.TailAppLog(limit)
	if err != nil {
		sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to read logs: %v", err))
		return
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"lines": lines,
	})
}

func (h *Handler) handleRoutes(w http.ResponseWriter, r *http.Request) {
	if !h.validateAdminAPIKey(r) {
		sendError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}

	if r.Method != http.MethodGet {
		sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	cfg := h.Config.Get()
	sendJSON(w, http.StatusOK, cfg.Routes)
}

func (h *Handler) handleProviders(w http.ResponseWriter, r *http.Request) {
	if !h.validateAdminAPIKey(r) {
		sendError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}

	if r.Method != http.MethodGet {
		sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	cfg := h.Config.Get()
	sendJSON(w, http.StatusOK, cfg.Providers)
}
