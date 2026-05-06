package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mark0725/go-agents-proxy/internal/config"
	"github.com/mark0725/go-agents-proxy/internal/logger"
)

// Handler holds dependencies for proxy endpoints.
type Handler struct {
	Config *config.Manager
	Logger *logger.Logger
}

// NewHandler creates a new proxy handler.
func NewHandler(cfg *config.Manager, log *logger.Logger) *Handler {
	return &Handler{Config: cfg, Logger: log}
}

// validateAPIKey checks the request against configured tokens.
func (h *Handler) validateAPIKey(r *http.Request) bool {
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
	for _, t := range cfg.Tokens {
		if t.Token == apiKey {
			return true
		}
	}
	return false
}

func sendAuthError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":    "authentication_error",
			"message": "Invalid API key.",
		},
	})
}

func sendError(w http.ResponseWriter, status int, message string) {
	slog.Error("proxy error", slog.Int("status", status), slog.String("message", message))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":    "api_error",
			"message": message,
		},
	})
}

func extractRouteID(path string) string {
	// path is /llm/<route-id>/v1/messages or /llm/<route-id>/v1/messages/count_tokens
	parts := strings.Split(path, "/")
	if len(parts) >= 3 && parts[1] == "llm" {
		return parts[2]
	}
	return ""
}

// HandleMessages processes /llm/<route-id>/v1/messages.
func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if !h.validateAPIKey(r) {
		slog.Warn("authentication failed", slog.String("path", r.URL.Path), slog.String("remote_addr", r.RemoteAddr))
		sendAuthError(w)
		return
	}

	if r.Method != http.MethodPost {
		sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	routeID := extractRouteID(r.URL.Path)
	if routeID == "" {
		sendError(w, http.StatusBadRequest, "Invalid route")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		sendError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}

	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to parse request: %v", err))
		return
	}

	cfg := h.Config.Get()
	targets, err := ResolveTargets(cfg, routeID, req.Model)
	if err != nil {
		sendError(w, http.StatusNotFound, err.Error())
		return
	}

	if req.Stream {
		h.handleStreamingWithFailover(w, r, &req, body, routeID, targets)
		return
	}

	h.handleNonStreamingWithFailover(w, r, &req, body, routeID, targets)
}

func (h *Handler) handleNonStreamingWithFailover(w http.ResponseWriter, r *http.Request, req *MessagesRequest, body []byte, routeID string, targets []ResolvedTarget) {
	var lastErr error
	var lastStatus int
	// Extract the path suffix after /llm/<route-id> (e.g. "/v1/messages")
	suffix := strings.TrimPrefix(r.URL.Path, "/llm/"+routeID)

	for _, target := range targets {
		start := time.Now()
		record := logger.LLMCallRecord{
			Timestamp:   start.Format(time.RFC3339),
			RouteID:     routeID,
			ModelID:     req.Model,
			Provider:    target.Provider,
			TargetModel: target.ModelID,
		}

		if target.APIType == "anthropic" {
			// Direct proxy for Anthropic: pass through the request path suffix.
			endpoint := BuildEndpoint(target.API.BaseURL, suffix)
			resp, err := CallProvider(r.Context(), target.API, body, target.Proxy, endpoint)
			if err != nil {
				lastErr = err
				record.Error = err.Error()
				record.DurationMs = time.Since(start).Milliseconds()
				h.Logger.LogLLMCall(record)
				continue
			}
			respBody, err := ReadResponseBody(resp)
			record.StatusCode = resp.StatusCode
			record.DurationMs = time.Since(start).Milliseconds()
			if err != nil {
				lastErr = err
				record.Error = err.Error()
				h.Logger.LogLLMCall(record)
				continue
			}
			if resp.StatusCode != http.StatusOK {
				lastErr = fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
				lastStatus = resp.StatusCode
				record.Error = lastErr.Error()
				h.Logger.LogLLMCall(record)
				if !IsRetryableError(nil, resp.StatusCode) {
					break
				}
				continue
			}
			// Copy response headers
			for key, values := range resp.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(respBody)
			h.Logger.LogLLMCall(record)
			return
		}

		// Convert Anthropic -> OpenAI
		openAIReq, err := ConvertAnthropicToOpenAI(req, Provider(target.APIType))
		if err != nil {
			sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to convert request: %v", err))
			return
		}
		openAIReq.Model = target.ModelID

		jsonBody, err := json.Marshal(openAIReq)
		if err != nil {
			sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to marshal request: %v", err))
			return
		}

		endpoint := target.API.BaseURL + "/chat/completions"
		resp, err := CallProvider(r.Context(), target.API, jsonBody, target.Proxy, endpoint)
		if err != nil {
			lastErr = err
			record.Error = err.Error()
			record.DurationMs = time.Since(start).Milliseconds()
			h.Logger.LogLLMCall(record)
			continue
		}
		respBody, err := ReadResponseBody(resp)
		record.StatusCode = resp.StatusCode
		record.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			lastErr = err
			record.Error = err.Error()
			h.Logger.LogLLMCall(record)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
			lastStatus = resp.StatusCode
			record.Error = lastErr.Error()
			h.Logger.LogLLMCall(record)
			if !IsRetryableError(nil, resp.StatusCode) {
				break
			}
			continue
		}

		var openAIResp OpenAIResponse
		if err := json.Unmarshal(respBody, &openAIResp); err != nil {
			lastErr = fmt.Errorf("failed to decode response: %w", err)
			record.Error = lastErr.Error()
			h.Logger.LogLLMCall(record)
			continue
		}

		anthropicResp := ConvertOpenAIToAnthropic(&openAIResp, req, Provider(target.APIType))
		record.InputTokens = anthropicResp.Usage.InputTokens
		record.OutputTokens = anthropicResp.Usage.OutputTokens
		h.Logger.LogLLMCall(record)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResp)
		return
	}

	// All targets failed
	if lastStatus > 0 {
		sendError(w, lastStatus, lastErr.Error())
	} else {
		sendError(w, http.StatusInternalServerError, fmt.Sprintf("All providers failed: %v", lastErr))
	}
}

func (h *Handler) handleStreamingWithFailover(w http.ResponseWriter, r *http.Request, req *MessagesRequest, body []byte, routeID string, targets []ResolvedTarget) {
	// For streaming, we cannot retry after starting to write the response.
	// We try the first target; on failure we cannot fall back.
	// A future improvement could do a lightweight health check before choosing.
	if len(targets) == 0 {
		sendError(w, http.StatusInternalServerError, "No targets available")
		return
	}

	target := targets[0]
	start := time.Now()
	record := logger.LLMCallRecord{
		Timestamp:   start.Format(time.RFC3339),
		RouteID:     routeID,
		ModelID:     req.Model,
		Provider:    target.Provider,
		TargetModel: target.ModelID,
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/llm/"+routeID)

	if target.APIType == "anthropic" {
		slog.Info("stream request",
			slog.String("route", routeID),
			slog.String("model", req.Model),
			slog.String("provider", target.Provider),
		)
		HandleAnthropicStreamProxy(r.Context(), w, target.API, target.Proxy, body, suffix)
		record.DurationMs = time.Since(start).Milliseconds()
		h.Logger.LogLLMCall(record)
		return
	}

	openAIReq, err := ConvertAnthropicToOpenAI(req, Provider(target.APIType))
	if err != nil {
		sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to convert request: %v", err))
		return
	}
	openAIReq.Model = target.ModelID

	slog.Info("stream request",
		slog.String("route", routeID),
		slog.String("model", req.Model),
		slog.String("provider", target.Provider),
	)

	HandleOpenAIStream(r.Context(), w, req, openAIReq, target.API, target.Proxy, h.Logger, routeID)
	record.DurationMs = time.Since(start).Milliseconds()
	h.Logger.LogLLMCall(record)
}

// HandleCountTokens processes /llm/<route-id>/v1/messages/count_tokens.
func (h *Handler) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	if !h.validateAPIKey(r) {
		slog.Warn("authentication failed", slog.String("path", r.URL.Path), slog.String("remote_addr", r.RemoteAddr))
		sendAuthError(w)
		return
	}

	if r.Method != http.MethodPost {
		sendError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	routeID := extractRouteID(r.URL.Path)
	if routeID == "" {
		sendError(w, http.StatusBadRequest, "Invalid route")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		sendError(w, http.StatusBadRequest, "Failed to read request body")
		return
	}

	var req TokenCountRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to parse request: %v", err))
		return
	}

	cfg := h.Config.Get()
	route, ok := cfg.Routes[routeID]
	if !ok {
		sendError(w, http.StatusNotFound, fmt.Sprintf("Route not found: %s", routeID))
		return
	}

	if route.APIType == "anthropic" {
		// Proxy to Anthropic count_tokens
		targets, err := ResolveTargets(cfg, routeID, req.Model)
		if err != nil || len(targets) == 0 {
			sendError(w, http.StatusNotFound, "No targets available")
			return
		}
		target := targets[0]
		suffix := strings.TrimPrefix(r.URL.Path, "/llm/"+routeID)
			endpoint := BuildEndpoint(target.API.BaseURL, suffix)
		httpReq, err := http.NewRequestWithContext(r.Context(), "POST", endpoint, strings.NewReader(string(body)))
		if err != nil {
			sendError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", target.API.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		var resp *http.Response
		if target.Proxy != "" {
			client := &http.Client{
				Transport: &http.Transport{
					Proxy: func(*http.Request) (*url.URL, error) {
						return url.Parse(target.Proxy)
					},
				},
			}
			resp, err = client.Do(httpReq)
		} else {
			resp, err = sharedHTTPClient.Do(httpReq)
		}
		if err != nil {
			sendError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	// Rough estimate for OpenAI/Google
	totalChars := 0
	for _, msg := range req.Messages {
		if content, ok := msg.Content.(string); ok {
			totalChars += len(content)
		}
	}

	estimatedTokens := totalChars / 4
	if estimatedTokens < 100 {
		estimatedTokens = 100
	}

	resp := TokenCountResponse{InputTokens: estimatedTokens}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
