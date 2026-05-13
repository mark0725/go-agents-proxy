package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
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

// validateLLMAPIKey checks /llm requests against configured tokens only.
func (h *Handler) validateLLMAPIKey(r *http.Request) bool {
	cfg := h.Config.Get()
	apiKey := r.Header.Get("x-api-key")
	if apiKey == "" {
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			apiKey = strings.TrimPrefix(authHeader, "Bearer ")
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

func copyResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func copyNormalizedResponseHeaders(dst, src http.Header) {
	allowed := map[string]struct{}{
		"X-Request-Id":         {},
		"Request-Id":           {},
		"X-Trace-Id":           {},
		"Traceparent":          {},
		"Tracestate":           {},
		"Anthropic-Request-Id": {},
	}
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if _, ok := allowed[canonical]; !ok {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func rewriteModelInJSONBody(body []byte, modelID string) ([]byte, error) {
	var reqBody map[string]interface{}
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return nil, err
	}
	reqBody["model"] = modelID
	return json.Marshal(reqBody)
}

func rewritePassthroughBody(clientAPIType string, body []byte, modelID string) ([]byte, error) {
	if clientAPIType == "google" {
		var reqBody map[string]interface{}
		if err := json.Unmarshal(body, &reqBody); err != nil {
			return nil, err
		}
		if _, ok := reqBody["model"]; ok {
			reqBody["model"] = modelID
			return json.Marshal(reqBody)
		}
		return body, nil
	}
	return rewriteModelInJSONBody(body, modelID)
}

func shouldPassthrough(target ResolvedTarget) bool {
	return target.ClientAPIType != "" && target.ClientAPIType == target.ProviderAPIType
}

func extractRouteID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "llm" {
		return parts[1]
	}
	return ""
}

func formatLogBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err == nil {
		return truncateLogText(pretty.String())
	}
	return truncateLogText(string(body))
}

func marshalLogValue(v interface{}) string {
	if v == nil {
		return ""
	}
	body, err := json.Marshal(v)
	if err != nil {
		return truncateLogText(fmt.Sprintf("%v", v))
	}
	return formatLogBody(body)
}

func truncateLogText(text string) string {
	const maxLen = 8000
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "\n... (truncated)"
}

type protocolSpec struct {
	MessagesSuffix    string
	CountTokensSuffix string
	StreamHeader      string
}

var protocolSpecs = map[string]protocolSpec{
	"anthropic": {
		MessagesSuffix:    "/v1/messages",
		CountTokensSuffix: "/v1/messages/count_tokens",
		StreamHeader:      "text/event-stream",
	},
	"openai": {
		MessagesSuffix:    "/chat/completions",
		CountTokensSuffix: "/chat/completions",
		StreamHeader:      "text/event-stream",
	},
	"google": {
		MessagesSuffix:    "/v1beta/models/{model}:generateContent",
		CountTokensSuffix: "/v1beta/models/{model}:countTokens",
		StreamHeader:      "text/event-stream",
	},
}

type requestContext struct {
	RouteID       string
	ClientAPIType string
	PathType      string
	Body          []byte
	OriginalPath  string
}

func (h *Handler) HandleLLM(w http.ResponseWriter, r *http.Request) {
	if !h.validateLLMAPIKey(r) {
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

	cfg := h.Config.Get()
	route, ok := cfg.Routes[routeID]
	if !ok {
		sendError(w, http.StatusNotFound, fmt.Sprintf("Route not found: %s", routeID))
		return
	}

	ctx, err := h.parseRequestContext(r, routeID, normalizeAPIType(route.APIType))
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch ctx.PathType {
	case "count_tokens":
		h.handleCountTokens(w, r, ctx)
	default:
		h.handleMessages(w, r, ctx)
	}
}

func (h *Handler) parseRequestContext(r *http.Request, routeID, clientAPIType string) (*requestContext, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body")
	}

	relPath := strings.TrimPrefix(r.URL.Path, "/llm/"+routeID)
	pathType, err := detectPathType(relPath, clientAPIType)
	if err != nil {
		return nil, err
	}

	return &requestContext{
		RouteID:       routeID,
		ClientAPIType: clientAPIType,
		PathType:      pathType,
		Body:          body,
		OriginalPath:  relPath,
	}, nil
}

func detectPathType(relPath, clientAPIType string) (string, error) {
	relPath = strings.TrimSpace(relPath)
	switch clientAPIType {
	case "anthropic":
		if strings.HasSuffix(relPath, "/count_tokens") {
			return "count_tokens", nil
		}
		if strings.HasSuffix(relPath, "/messages") {
			return "messages", nil
		}
	case "openai":
		if strings.HasSuffix(relPath, "/responses") {
			return "responses", nil
		}
		if strings.HasSuffix(relPath, "/chat/completions") {
			return "messages", nil
		}
	case "google":
		if strings.Contains(relPath, ":countTokens") {
			return "count_tokens", nil
		}
		if strings.Contains(relPath, ":generateContent") || strings.Contains(relPath, ":streamGenerateContent") {
			return "messages", nil
		}
	}
	return "", fmt.Errorf("unsupported %s path: %s", clientAPIType, relPath)
}

func extractModelFromBody(body []byte) (string, error) {
	var reqBody map[string]interface{}
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return "", err
	}
	model, _ := reqBody["model"].(string)
	if model == "" {
		return "", fmt.Errorf("request model is required")
	}
	return model, nil
}

func extractGoogleModelFromPath(relPath string) string {
	modelPath := relPath
	if idx := strings.Index(modelPath, "/models/"); idx >= 0 {
		modelPath = modelPath[idx+len("/models/"):]
	}
	if idx := strings.Index(modelPath, ":"); idx >= 0 {
		modelPath = modelPath[:idx]
	}
	return strings.TrimSpace(modelPath)
}

func extractModelFromRequest(body []byte, clientAPIType, relPath string) (string, error) {
	modelID, err := extractModelFromBody(body)
	if err == nil && modelID != "" {
		return modelID, nil
	}
	if clientAPIType == "google" {
		modelID = extractGoogleModelFromPath(relPath)
		if modelID != "" {
			return modelID, nil
		}
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("request model is required")
}

func isStreamingRequest(body []byte, clientAPIType string, relPath string) bool {
	if clientAPIType == "google" {
		return strings.Contains(relPath, ":streamGenerateContent")
	}
	var reqBody map[string]interface{}
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return false
	}
	stream, _ := reqBody["stream"].(bool)
	return stream
}

func (h *Handler) handleMessages(w http.ResponseWriter, r *http.Request, ctx *requestContext) {
	modelID, err := extractModelFromRequest(ctx.Body, ctx.ClientAPIType, ctx.OriginalPath)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.Config.Get()
	targets, err := ResolveTargets(cfg, ctx.RouteID, modelID)
	if err != nil {
		sendError(w, http.StatusNotFound, err.Error())
		return
	}

	if isStreamingRequest(ctx.Body, ctx.ClientAPIType, ctx.OriginalPath) {
		h.handleStreamingWithFailover(w, r, ctx, modelID, targets)
		return
	}

	h.handleNonStreamingWithFailover(w, r, ctx, modelID, targets)
}

func (h *Handler) handleNonStreamingWithFailover(w http.ResponseWriter, r *http.Request, ctx *requestContext, originalModelID string, targets []ResolvedTarget) {
	var lastErr error
	var lastStatus int

	for _, target := range targets {
		start := time.Now()
		record := logger.LLMCallRecord{
			Timestamp:   start.Format(time.RFC3339),
			RouteID:     ctx.RouteID,
			ModelID:     originalModelID,
			Provider:    target.Provider,
			TargetModel: target.ModelID,
			RequestBody: formatLogBody(ctx.Body),
		}

		var requestBody []byte
		var anthropicReq *MessagesRequest
		var err error

		if shouldPassthrough(target) {
			requestBody, err = rewritePassthroughBody(ctx.ClientAPIType, ctx.Body, target.ModelID)
			if err != nil {
				sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to rewrite request: %v", err))
				return
			}
		} else {
			anthropicReq, err = convertRequestToAnthropic(ctx.ClientAPIType, ctx.PathType, ctx.Body, ctx.OriginalPath)
			if err != nil {
				sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to normalize request: %v", err))
				return
			}
			requestBody, err = buildProviderMessagesBody(anthropicReq, target)
			if err != nil {
				sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to convert request: %v", err))
				return
			}
		}

		record.RequestBody = formatLogBody(requestBody)
		endpoint := buildProviderEndpoint(target, ctx.PathType, target.ModelID, false)
		resp, err := CallProvider(r.Context(), target.API, requestBody, target.Proxy, endpoint, r.Header)
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
			record.ResponseBody = formatLogBody(respBody)
			h.Logger.LogLLMCall(record)
			if !IsRetryableError(nil, resp.StatusCode) {
				break
			}
			continue
		}

		if shouldPassthrough(target) {
			record.ResponseBody = formatLogBody(respBody)
			copyResponse(w, resp, respBody)
			h.Logger.LogLLMCall(record)
			return
		}

		anthropicResp, err := normalizeProviderResponse(target.ProviderAPIType, anthropicReq, respBody)
		if err != nil {
			lastErr = err
			record.Error = err.Error()
			record.ResponseBody = formatLogBody(respBody)
			h.Logger.LogLLMCall(record)
			continue
		}

		record.InputTokens = anthropicResp.Usage.InputTokens
		record.OutputTokens = anthropicResp.Usage.OutputTokens
		if anthropicResp.StopReason != nil {
			record.StopReason = *anthropicResp.StopReason
		}
		record.ResponseBody = marshalLogValue(anthropicResp)
		copyNormalizedResponseHeaders(w.Header(), resp.Header)
		writeNormalizedResponse(w, ctx.ClientAPIType, ctx.PathType, anthropicResp)
		h.Logger.LogLLMCall(record)
		return
	}

	if lastStatus > 0 {
		sendError(w, lastStatus, lastErr.Error())
	} else {
		sendError(w, http.StatusInternalServerError, fmt.Sprintf("All providers failed: %v", lastErr))
	}
}

func (h *Handler) handleStreamingWithFailover(w http.ResponseWriter, r *http.Request, ctx *requestContext, originalModelID string, targets []ResolvedTarget) {
	if len(targets) == 0 {
		sendError(w, http.StatusInternalServerError, "No targets available")
		return
	}

	target := targets[0]
	start := time.Now()
	record := logger.LLMCallRecord{
		Timestamp:   start.Format(time.RFC3339),
		RouteID:     ctx.RouteID,
		ModelID:     originalModelID,
		Provider:    target.Provider,
		TargetModel: target.ModelID,
		RequestBody: formatLogBody(ctx.Body),
	}

	if shouldPassthrough(target) {
		jsonBody, err := rewritePassthroughBody(ctx.ClientAPIType, ctx.Body, target.ModelID)
		if err != nil {
			sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to rewrite request: %v", err))
			return
		}
		record.RequestBody = formatLogBody(jsonBody)
		slog.Info("stream request",
			slog.String("route", ctx.RouteID),
			slog.String("model", originalModelID),
			slog.String("provider", target.Provider),
		)
		var streamResult *StreamResult
		if target.ClientAPIType == "anthropic" {
			streamResult, err = HandleAnthropicStreamProxy(r.Context(), w, target, target.Proxy, r.Header, jsonBody)
		} else {
			streamResult, err = HandleProtocolStreamProxy(r.Context(), w, target, target.Proxy, r.Header, jsonBody, ctx.PathType, target.ModelID)
		}
		if err != nil {
			record.Error = err.Error()
		}
		if streamResult != nil {
			record.StatusCode = streamResult.StatusCode
			record.InputTokens = streamResult.InputTokens
			record.OutputTokens = streamResult.OutputTokens
			record.StopReason = streamResult.StopReason
			record.ResponseBody = streamResult.ResponseBody
		}
		record.DurationMs = time.Since(start).Milliseconds()
		h.Logger.LogLLMCall(record)
		return
	}

	anthropicReq, err := convertRequestToAnthropic(ctx.ClientAPIType, ctx.PathType, ctx.Body, ctx.OriginalPath)
	if err != nil {
		sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to normalize request: %v", err))
		return
	}

	slog.Info("stream request",
		slog.String("route", ctx.RouteID),
		slog.String("model", originalModelID),
		slog.String("provider", target.Provider),
	)

	switch target.ProviderAPIType {
	case "openai":
		openAIReq, err := ConvertAnthropicToOpenAI(anthropicReq, Provider(target.ProviderAPIType))
		if err != nil {
			sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to convert request: %v", err))
			return
		}
		openAIReq.Model = target.ModelID
		streamResult, err := HandleOpenAIStream(r.Context(), w, anthropicReq, openAIReq, target.API, target.Proxy, r.Header, h.Logger, ctx.RouteID)
		if err != nil {
			record.Error = err.Error()
		}
		if streamResult != nil {
			record.StatusCode = streamResult.StatusCode
			record.InputTokens = streamResult.InputTokens
			record.OutputTokens = streamResult.OutputTokens
			record.StopReason = streamResult.StopReason
			record.ResponseBody = streamResult.ResponseBody
		}
		record.DurationMs = time.Since(start).Milliseconds()
		h.Logger.LogLLMCall(record)
		return
	case "google":
		if ctx.ClientAPIType != "anthropic" {
			sendError(w, http.StatusBadRequest, "streaming cross-type conversion is only supported for anthropic clients")
			return
		}
		googleBody, err := convertAnthropicToGoogleBody(anthropicReq, target.ModelID)
		if err != nil {
			sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to convert request: %v", err))
			return
		}
		record.RequestBody = formatLogBody(googleBody)
		streamResult, err := HandleGoogleStreamAsAnthropic(r.Context(), w, anthropicReq, target, target.Proxy, r.Header, googleBody)
		if err != nil {
			record.Error = err.Error()
		}
		if streamResult != nil {
			record.StatusCode = streamResult.StatusCode
			record.InputTokens = streamResult.InputTokens
			record.OutputTokens = streamResult.OutputTokens
			record.StopReason = streamResult.StopReason
			record.ResponseBody = streamResult.ResponseBody
		}
		record.DurationMs = time.Since(start).Milliseconds()
		h.Logger.LogLLMCall(record)
		return
	default:
		sendError(w, http.StatusBadRequest, "streaming cross-type conversion is only supported for openai/google providers")
		return
	}
}

func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request, ctx *requestContext) {
	modelID, err := extractModelFromRequest(ctx.Body, ctx.ClientAPIType, ctx.OriginalPath)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := h.Config.Get()
	targets, err := ResolveTargets(cfg, ctx.RouteID, modelID)
	if err != nil || len(targets) == 0 {
		sendError(w, http.StatusNotFound, "No targets available")
		return
	}
	target := targets[0]

	if shouldPassthrough(target) {
		jsonBody, err := rewritePassthroughBody(ctx.ClientAPIType, ctx.Body, target.ModelID)
		if err != nil {
			sendError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to rewrite request: %v", err))
			return
		}
		endpoint := buildProviderEndpoint(target, ctx.PathType, target.ModelID, false)
		resp, err := CallProvider(r.Context(), target.API, jsonBody, target.Proxy, endpoint, r.Header)
		if err != nil {
			sendError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respBody, err := ReadResponseBody(resp)
		if err != nil {
			sendError(w, http.StatusInternalServerError, err.Error())
			return
		}
		copyResponse(w, resp, respBody)
		return
	}

	anthropicReq, err := convertTokenCountRequestToAnthropic(ctx.ClientAPIType, ctx.PathType, ctx.Body, ctx.OriginalPath)
	if err != nil {
		sendError(w, http.StatusBadRequest, fmt.Sprintf("Failed to normalize token request: %v", err))
		return
	}

	estimatedTokens := estimateTokenCount(anthropicReq)

	resp := TokenCountResponse{InputTokens: estimatedTokens}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func estimateTokenCount(req *TokenCountRequest) int {
	totalChars := estimateContentChars(req.System)
	for _, msg := range req.Messages {
		totalChars += len(msg.Role)
		totalChars += estimateContentChars(msg.Content)
	}
	for _, tool := range req.Tools {
		totalChars += len(tool.Name)
		totalChars += len(tool.Description)
		if schemaBytes, err := json.Marshal(tool.InputSchema); err == nil {
			totalChars += len(schemaBytes)
		}
	}
	if req.ToolChoice != nil {
		if body, err := json.Marshal(req.ToolChoice); err == nil {
			totalChars += len(body)
		}
	}
	if req.Thinking != nil {
		if body, err := json.Marshal(req.Thinking); err == nil {
			totalChars += len(body)
		}
	}
	estimatedTokens := totalChars / 4
	if estimatedTokens < 100 {
		return 100
	}
	return estimatedTokens
}

func estimateContentChars(content interface{}) int {
	switch v := content.(type) {
	case nil:
		return 0
	case string:
		return len(v)
	case []interface{}:
		total := 0
		for _, item := range v {
			total += estimateContentChars(item)
		}
		return total
	case map[string]interface{}:
		total := 0
		if text, ok := v["text"].(string); ok {
			total += len(text)
		}
		if content, ok := v["content"]; ok {
			total += estimateContentChars(content)
		}
		if input, ok := v["input"]; ok {
			if body, err := json.Marshal(input); err == nil {
				total += len(body)
			}
		}
		if output, ok := v["output"]; ok {
			total += estimateContentChars(output)
		}
		if response, ok := v["response"]; ok {
			total += estimateContentChars(response)
		}
		if name, ok := v["name"].(string); ok {
			total += len(name)
		}
		if arguments, ok := v["arguments"].(string); ok {
			total += len(arguments)
		}
		return total
	default:
		if body, err := json.Marshal(v); err == nil {
			return len(body)
		}
		return 0
	}
}

func buildProviderEndpoint(target ResolvedTarget, pathType, modelID string, streaming bool) string {
	spec := protocolSpecs[target.ProviderAPIType]
	switch target.ProviderAPIType {
	case "anthropic":
		if pathType == "count_tokens" {
			return BuildEndpoint(target.API.BaseURL, spec.CountTokensSuffix)
		}
		return BuildEndpoint(target.API.BaseURL, spec.MessagesSuffix)
	case "openai":
		if pathType == "responses" {
			return BuildEndpoint(target.API.BaseURL, "/responses")
		}
		return BuildEndpoint(target.API.BaseURL, spec.MessagesSuffix)
	case "google":
		suffix := spec.MessagesSuffix
		if pathType == "count_tokens" {
			suffix = spec.CountTokensSuffix
		}
		if streaming {
			suffix = "/v1beta/models/{model}:streamGenerateContent?alt=sse"
		}
		suffix = strings.ReplaceAll(suffix, "{model}", modelID)
		return BuildEndpoint(target.API.BaseURL, suffix)
	default:
		return BuildEndpoint(target.API.BaseURL, path.Join("/", pathType))
	}
}

func buildProviderMessagesBody(anthropicReq *MessagesRequest, target ResolvedTarget) ([]byte, error) {
	switch target.ProviderAPIType {
	case "anthropic":
		forwardReq := *anthropicReq
		forwardReq.Model = target.ModelID
		return json.Marshal(&forwardReq)
	case "openai":
		openAIReq, err := ConvertAnthropicToOpenAI(anthropicReq, Provider(target.ProviderAPIType))
		if err != nil {
			return nil, err
		}
		openAIReq.Model = target.ModelID
		return json.Marshal(openAIReq)
	case "google":
		return convertAnthropicToGoogleBody(anthropicReq, target.ModelID)
	default:
		return nil, fmt.Errorf("unsupported provider api type: %s", target.ProviderAPIType)
	}
}

func writeNormalizedResponse(w http.ResponseWriter, clientAPIType, pathType string, anthropicResp *MessagesResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	switch clientAPIType {
	case "anthropic":
		json.NewEncoder(w).Encode(anthropicResp)
	case "openai":
		if pathType == "responses" {
			json.NewEncoder(w).Encode(convertAnthropicToOpenAIResponsesResponse(anthropicResp))
			return
		}
		json.NewEncoder(w).Encode(convertAnthropicToOpenAIChatResponse(anthropicResp))
	case "google":
		json.NewEncoder(w).Encode(convertAnthropicToGoogleResponse(anthropicResp))
	default:
		json.NewEncoder(w).Encode(anthropicResp)
	}
}

func convertRequestToAnthropic(clientAPIType, pathType string, body []byte, requestPath string) (*MessagesRequest, error) {
	switch clientAPIType {
	case "anthropic":
		var req MessagesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return &req, nil
	case "openai":
		return convertOpenAIRequestToAnthropic(body, pathType)
	case "google":
		return convertGoogleRequestToAnthropic(body, requestPath)
	default:
		return nil, fmt.Errorf("unsupported client api type: %s", clientAPIType)
	}
}

func convertTokenCountRequestToAnthropic(clientAPIType, pathType string, body []byte, requestPath string) (*TokenCountRequest, error) {
	switch clientAPIType {
	case "anthropic":
		var req TokenCountRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return &req, nil
	case "openai":
		anthropicReq, err := convertOpenAIRequestToAnthropic(body, pathType)
		if err != nil {
			return nil, err
		}
		return &TokenCountRequest{Model: anthropicReq.Model, Messages: anthropicReq.Messages, System: anthropicReq.System, Tools: anthropicReq.Tools, ToolChoice: anthropicReq.ToolChoice, Thinking: anthropicReq.Thinking}, nil
	case "google":
		anthropicReq, err := convertGoogleRequestToAnthropic(body, requestPath)
		if err != nil {
			return nil, err
		}
		return &TokenCountRequest{Model: anthropicReq.Model, Messages: anthropicReq.Messages, System: anthropicReq.System, Tools: anthropicReq.Tools, ToolChoice: anthropicReq.ToolChoice, Thinking: anthropicReq.Thinking}, nil
	default:
		return nil, fmt.Errorf("unsupported client api type: %s", clientAPIType)
	}
}

func convertOpenAIRequestToAnthropic(body []byte, pathType string) (*MessagesRequest, error) {
	if pathType == "responses" {
		return convertOpenAIResponsesRequestToAnthropic(body)
	}

	var req struct {
		Model               string          `json:"model"`
		Messages            []OpenAIMessage `json:"messages"`
		MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
		MaxTokens           int             `json:"max_tokens,omitempty"`
		Temperature         *float64        `json:"temperature,omitempty"`
		TopP                *float64        `json:"top_p,omitempty"`
		Tools               []OpenAITool    `json:"tools,omitempty"`
		ToolChoice          interface{}     `json:"tool_choice,omitempty"`
		Stream              bool            `json:"stream,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}

	anthropicReq := &MessagesRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}
	if req.MaxCompletionTokens > 0 {
		anthropicReq.MaxTokens = req.MaxCompletionTokens
	} else {
		anthropicReq.MaxTokens = req.MaxTokens
	}
	for _, msg := range req.Messages {
		anthropicReq.Messages = append(anthropicReq.Messages, Message{Role: msg.Role, Content: convertOpenAIMessageContentToAnthropic(msg.Content)})
	}
	for _, tool := range req.Tools {
		anthropicReq.Tools = append(anthropicReq.Tools, Tool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters})
	}
	anthropicReq.ToolChoice = convertOpenAIToolChoiceToAnthropic(req.ToolChoice)
	return anthropicReq, nil
}

func convertOpenAIResponsesRequestToAnthropic(body []byte) (*MessagesRequest, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}

	anthropicReq := &MessagesRequest{}
	if model, ok := req["model"].(string); ok {
		anthropicReq.Model = model
	}
	if maxOutputTokens, ok := req["max_output_tokens"].(float64); ok {
		anthropicReq.MaxTokens = int(maxOutputTokens)
	} else if maxTokens, ok := req["max_tokens"].(float64); ok {
		anthropicReq.MaxTokens = int(maxTokens)
	}
	if temperature, ok := req["temperature"].(float64); ok {
		anthropicReq.Temperature = &temperature
	}
	if topP, ok := req["top_p"].(float64); ok {
		anthropicReq.TopP = &topP
	}
	if stream, ok := req["stream"].(bool); ok {
		anthropicReq.Stream = stream
	}
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		anthropicReq.System = instructions
	}
	if tools, ok := req["tools"].([]interface{}); ok {
		anthropicReq.Tools = convertOpenAIResponsesToolsToAnthropic(tools)
	}
	anthropicReq.ToolChoice = convertOpenAIToolChoiceToAnthropic(req["tool_choice"])
	anthropicReq.Messages = convertOpenAIResponsesInputToAnthropicMessages(req["input"])
	return anthropicReq, nil
}

func convertOpenAIResponsesToolsToAnthropic(tools []interface{}) []Tool {
	result := make([]Tool, 0, len(tools))
	for _, item := range tools {
		toolMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if typ, _ := toolMap["type"].(string); typ != "function" {
			continue
		}
		tool := Tool{}
		if name, ok := toolMap["name"].(string); ok {
			tool.Name = name
		}
		if desc, ok := toolMap["description"].(string); ok {
			tool.Description = desc
		}
		if params, ok := toolMap["parameters"].(map[string]interface{}); ok {
			tool.InputSchema = params
		} else {
			tool.InputSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		if tool.Name != "" {
			result = append(result, tool)
		}
	}
	return result
}

func convertOpenAIToolChoiceToAnthropic(toolChoice interface{}) map[string]interface{} {
	if toolChoice == nil {
		return nil
	}
	switch choice := toolChoice.(type) {
	case string:
		switch choice {
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "required", "any":
			return map[string]interface{}{"type": "any"}
		case "none":
			return nil
		}
	case map[string]interface{}:
		if choice["type"] == "function" {
			if fn, ok := choice["function"].(map[string]interface{}); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					return map[string]interface{}{"type": "tool", "name": name}
				}
			}
		}
	}
	return nil
}

func convertOpenAIMessageContentToAnthropic(content interface{}) interface{} {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		blocks := make([]interface{}, 0, len(c))
		for _, item := range c {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := itemMap["type"].(string)
			switch typ {
			case "text", "input_text", "output_text":
				if text, ok := itemMap["text"].(string); ok {
					blocks = append(blocks, map[string]interface{}{"type": "text", "text": text})
				}
			case "image":
				blocks = append(blocks, itemMap)
			case "tool_result", "function_call_output":
				block := map[string]interface{}{"type": "tool_result"}
				if callID, ok := itemMap["call_id"].(string); ok {
					block["tool_use_id"] = callID
				} else if callID, ok := itemMap["tool_use_id"].(string); ok {
					block["tool_use_id"] = callID
				}
				if output, ok := itemMap["output"]; ok {
					block["content"] = output
				} else if output, ok := itemMap["content"]; ok {
					block["content"] = output
				}
				blocks = append(blocks, block)
			case "tool_use", "function_call":
				block := map[string]interface{}{"type": "tool_use"}
				if callID, ok := itemMap["call_id"].(string); ok {
					block["id"] = callID
				} else if callID, ok := itemMap["id"].(string); ok {
					block["id"] = callID
				}
				if name, ok := itemMap["name"].(string); ok {
					block["name"] = name
				}
				if args, ok := itemMap["arguments"].(map[string]interface{}); ok {
					block["input"] = args
				} else if argsJSON, ok := itemMap["arguments"].(string); ok {
					var args map[string]interface{}
					if json.Unmarshal([]byte(argsJSON), &args) == nil {
						block["input"] = args
					} else {
						block["input"] = map[string]interface{}{"raw": argsJSON}
					}
				}
				if _, ok := block["input"]; !ok {
					block["input"] = map[string]interface{}{}
				}
				blocks = append(blocks, block)
			}
		}
		if len(blocks) == 0 {
			return ""
		}
		return blocks
	default:
		return content
	}
}

func convertOpenAIResponsesInputToAnthropicMessages(input interface{}) []Message {
	messages := []Message{}
	switch v := input.(type) {
	case string:
		messages = append(messages, Message{Role: "user", Content: v})
	case []interface{}:
		for _, item := range v {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if typ, _ := itemMap["type"].(string); typ == "message" || itemMap["role"] != nil {
				role, _ := itemMap["role"].(string)
				if role == "" {
					role = "user"
				}
				content := itemMap["content"]
				if typ == "message" {
					content = convertOpenAIMessageContentToAnthropic(content)
				} else {
					content = convertOpenAIMessageContentToAnthropic([]interface{}{itemMap})
				}
				messages = append(messages, Message{Role: role, Content: content})
				continue
			}
			messages = append(messages, Message{Role: inferAnthropicRoleFromResponseItem(itemMap), Content: convertOpenAIMessageContentToAnthropic([]interface{}{itemMap})})
		}
	}
	return messages
}

func inferAnthropicRoleFromResponseItem(item map[string]interface{}) string {
	typ, _ := item["type"].(string)
	switch typ {
	case "function_call", "tool_use", "output_text":
		return "assistant"
	case "function_call_output", "input_text", "tool_result":
		return "user"
	default:
		return "user"
	}
}

func convertGoogleRequestToAnthropic(body []byte, requestPath string) (*MessagesRequest, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	anthropicReq := &MessagesRequest{}
	if model, ok := req["model"].(string); ok {
		anthropicReq.Model = model
	} else {
		anthropicReq.Model = extractGoogleModelFromPath(requestPath)
	}
	if systemInstruction, ok := req["systemInstruction"].(map[string]interface{}); ok {
		anthropicReq.System = googlePartsToAnthropicSystem(systemInstruction["parts"])
	} else if systemInstruction, ok := req["system_instruction"].(map[string]interface{}); ok {
		anthropicReq.System = googlePartsToAnthropicSystem(systemInstruction["parts"])
	}
	if generationConfig, ok := req["generationConfig"].(map[string]interface{}); ok {
		if maxTokens, ok := generationConfig["maxOutputTokens"].(float64); ok {
			anthropicReq.MaxTokens = int(maxTokens)
		}
		if temperature, ok := generationConfig["temperature"].(float64); ok {
			anthropicReq.Temperature = &temperature
		}
		if topP, ok := generationConfig["topP"].(float64); ok {
			anthropicReq.TopP = &topP
		}
		if stops, ok := generationConfig["stopSequences"].([]interface{}); ok {
			anthropicReq.StopSequences = interfaceSliceToStrings(stops)
		}
	}
	if contents, ok := req["contents"].([]interface{}); ok {
		anthropicReq.Messages = googleContentsToAnthropicMessages(contents)
	}
	if tools, ok := req["tools"].([]interface{}); ok {
		anthropicReq.Tools = googleToolsToAnthropic(tools)
	}
	if toolConfig, ok := req["toolConfig"].(map[string]interface{}); ok {
		anthropicReq.ToolChoice = googleToolChoiceToAnthropic(toolConfig)
	}
	return anthropicReq, nil
}

func googlePartsToAnthropicSystem(partsValue interface{}) interface{} {
	parts, ok := partsValue.([]interface{})
	if !ok {
		return nil
	}
	blocks := make([]interface{}, 0, len(parts))
	for _, part := range parts {
		partMap, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		if text, ok := partMap["text"].(string); ok {
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": text})
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return blocks
}

func googleContentsToAnthropicMessages(contents []interface{}) []Message {
	messages := make([]Message, 0, len(contents))
	toolNames := map[string]string{}
	for _, item := range contents {
		contentMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		role := "user"
		if value, ok := contentMap["role"].(string); ok && value != "" {
			if value == "model" {
				role = "assistant"
			} else {
				role = value
			}
		}
		message := Message{Role: role}
		if parts, ok := contentMap["parts"].([]interface{}); ok {
			blocks := make([]interface{}, 0, len(parts))
			for _, part := range parts {
				partMap, ok := part.(map[string]interface{})
				if !ok {
					continue
				}
				if text, ok := partMap["text"].(string); ok {
					blocks = append(blocks, map[string]interface{}{"type": "text", "text": text})
					continue
				}
				if functionCall, ok := partMap["functionCall"].(map[string]interface{}); ok {
					toolID := generateToolID()
					block := map[string]interface{}{
						"type":  "tool_use",
						"id":    toolID,
						"input": map[string]interface{}{},
					}
					if name, ok := functionCall["name"].(string); ok {
						block["name"] = name
						toolNames[toolID] = name
					}
					if args, ok := functionCall["args"].(map[string]interface{}); ok {
						block["input"] = args
					}
					blocks = append(blocks, block)
					continue
				}
				if functionResponse, ok := partMap["functionResponse"].(map[string]interface{}); ok {
					block := map[string]interface{}{"type": "tool_result"}
					if name, ok := functionResponse["name"].(string); ok {
						for toolID, toolName := range toolNames {
							if toolName == name {
								block["tool_use_id"] = toolID
								break
							}
						}
					}
					if response, ok := functionResponse["response"]; ok {
						block["content"] = response
					}
					blocks = append(blocks, block)
				}
			}
			message.Content = blocks
		}
		messages = append(messages, message)
	}
	return messages
}

func googleToolsToAnthropic(tools []interface{}) []Tool {
	result := []Tool{}
	for _, item := range tools {
		toolMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		declarations, ok := toolMap["functionDeclarations"].([]interface{})
		if !ok {
			continue
		}
		for _, decl := range declarations {
			declMap, ok := decl.(map[string]interface{})
			if !ok {
				continue
			}
			tool := Tool{InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}
			if name, ok := declMap["name"].(string); ok {
				tool.Name = name
			}
			if desc, ok := declMap["description"].(string); ok {
				tool.Description = desc
			}
			if params, ok := declMap["parameters"].(map[string]interface{}); ok {
				tool.InputSchema = params
			}
			if tool.Name != "" {
				result = append(result, tool)
			}
		}
	}
	return result
}

func googleToolChoiceToAnthropic(toolConfig map[string]interface{}) map[string]interface{} {
	functionCallingConfig, ok := toolConfig["functionCallingConfig"].(map[string]interface{})
	if !ok {
		return nil
	}
	mode, _ := functionCallingConfig["mode"].(string)
	switch mode {
	case "AUTO":
		return map[string]interface{}{"type": "auto"}
	case "ANY":
		allowed, _ := functionCallingConfig["allowedFunctionNames"].([]interface{})
		if len(allowed) == 1 {
			if name, ok := allowed[0].(string); ok {
				return map[string]interface{}{"type": "tool", "name": name}
			}
		}
		return map[string]interface{}{"type": "any"}
	default:
		return nil
	}
}

func interfaceSliceToStrings(values []interface{}) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if s, ok := value.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func convertAnthropicToGoogleBody(req *MessagesRequest, modelID string) ([]byte, error) {
	toolNames := map[string]string{}
	contents := make([]interface{}, 0, len(req.Messages))
	for _, msg := range req.Messages {
		parts := anthropicContentToGoogleParts(msg.Content, toolNames)
		role := msg.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]interface{}{
			"role":  role,
			"parts": parts,
		})
	}

	googleReq := map[string]interface{}{
		"contents": contents,
	}
	if req.System != nil {
		if systemParts := anthropicSystemToGoogleParts(req.System); len(systemParts) > 0 {
			googleReq["systemInstruction"] = map[string]interface{}{"parts": systemParts}
		}
	}
	generationConfig := map[string]interface{}{}
	if req.MaxTokens > 0 {
		generationConfig["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		generationConfig["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		generationConfig["topP"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		generationConfig["stopSequences"] = req.StopSequences
	}
	if len(generationConfig) > 0 {
		googleReq["generationConfig"] = generationConfig
	}
	if len(req.Tools) > 0 {
		declarations := make([]interface{}, 0, len(req.Tools))
		for _, tool := range req.Tools {
			declarations = append(declarations, map[string]interface{}{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  cleanGoogleSchema(tool.InputSchema),
			})
		}
		googleReq["tools"] = []interface{}{map[string]interface{}{"functionDeclarations": declarations}}
	}
	if req.ToolChoice != nil {
		toolConfig := map[string]interface{}{}
		switch req.ToolChoice["type"] {
		case "auto":
			toolConfig["functionCallingConfig"] = map[string]interface{}{"mode": "AUTO"}
		case "any":
			toolConfig["functionCallingConfig"] = map[string]interface{}{"mode": "ANY"}
		case "tool":
			if name, ok := req.ToolChoice["name"].(string); ok && name != "" {
				toolConfig["functionCallingConfig"] = map[string]interface{}{"mode": "ANY", "allowedFunctionNames": []string{name}}
			}
		}
		if len(toolConfig) > 0 {
			googleReq["toolConfig"] = toolConfig
		}
	}
	googleReq["model"] = modelID
	return json.Marshal(googleReq)
}

func anthropicContentToGoogleParts(content interface{}, toolNames map[string]string) []interface{} {
	parts := []interface{}{}
	switch c := content.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]interface{}{"text": c})
		}
	case []interface{}:
		for _, item := range c {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				if text, ok := block["text"].(string); ok {
					parts = append(parts, map[string]interface{}{"text": text})
				}
			case "tool_use":
				toolID, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if toolID != "" && name != "" {
					toolNames[toolID] = name
				}
				args := map[string]interface{}{}
				if input, ok := block["input"].(map[string]interface{}); ok {
					args = input
				}
				parts = append(parts, map[string]interface{}{"functionCall": map[string]interface{}{"name": name, "args": args}})
			case "tool_result":
				toolID, _ := block["tool_use_id"].(string)
				name := toolNames[toolID]
				response := map[string]interface{}{"content": block["content"]}
				parts = append(parts, map[string]interface{}{"functionResponse": map[string]interface{}{"name": name, "response": response}})
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{"text": ""})
	}
	return parts
}

func anthropicSystemToGoogleParts(system interface{}) []interface{} {
	parts := []interface{}{}
	switch s := system.(type) {
	case string:
		if s != "" {
			parts = append(parts, map[string]interface{}{"text": s})
		}
	case []interface{}:
		for _, item := range s {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, map[string]interface{}{"text": text})
				}
			}
		}
	}
	return parts
}

func normalizeProviderResponse(providerAPIType string, originalReq *MessagesRequest, respBody []byte) (*MessagesResponse, error) {
	switch providerAPIType {
	case "anthropic":
		var anthropicResp MessagesResponse
		if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
			return nil, err
		}
		return &anthropicResp, nil
	case "openai":
		var openAIResp OpenAIResponse
		if err := json.Unmarshal(respBody, &openAIResp); err != nil {
			return nil, err
		}
		return ConvertOpenAIToAnthropic(&openAIResp, originalReq, Provider(providerAPIType)), nil
	case "google":
		return convertGoogleResponseToAnthropic(respBody, originalReq.Model)
	default:
		return nil, fmt.Errorf("unsupported provider api type: %s", providerAPIType)
	}
}

func convertGoogleResponseToAnthropic(respBody []byte, originalModel string) (*MessagesResponse, error) {
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []map[string]interface{} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, err
	}
	content := []interface{}{}
	stopReason := "end_turn"
	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]
		for _, part := range candidate.Content.Parts {
			if text, ok := part["text"].(string); ok {
				content = append(content, map[string]interface{}{"type": "text", "text": text})
				continue
			}
			if functionCall, ok := part["functionCall"].(map[string]interface{}); ok {
				block := map[string]interface{}{
					"type":  "tool_use",
					"id":    generateToolID(),
					"input": map[string]interface{}{},
				}
				if name, ok := functionCall["name"].(string); ok {
					block["name"] = name
				}
				if args, ok := functionCall["args"].(map[string]interface{}); ok {
					block["input"] = args
				}
				content = append(content, block)
			}
		}
		switch candidate.FinishReason {
		case "MAX_TOKENS":
			stopReason = "max_tokens"
		case "STOP":
			stopReason = "end_turn"
		case "TOOL_CALL", "MALFORMED_FUNCTION_CALL":
			stopReason = "tool_use"
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]interface{}{"type": "text", "text": ""})
	}
	return &MessagesResponse{
		ID:         generateMessageID(),
		Model:      originalModel,
		Role:       "assistant",
		Content:    content,
		Type:       "message",
		StopReason: &stopReason,
		Usage: Usage{
			InputTokens:  resp.UsageMetadata.PromptTokenCount,
			OutputTokens: resp.UsageMetadata.CandidatesTokenCount,
		},
	}, nil
}

func convertAnthropicToOpenAIChatResponse(resp *MessagesResponse) map[string]interface{} {
	message := map[string]interface{}{"role": "assistant", "content": anthropicContentToPlainText(resp.Content)}
	toolCalls := anthropicContentToOpenAIToolCalls(resp.Content)
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	finishReason := anthropicStopReasonToOpenAI(resp.StopReason)
	return map[string]interface{}{
		"id":      resp.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   resp.Model,
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]interface{}{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

func convertAnthropicToOpenAIResponsesResponse(resp *MessagesResponse) map[string]interface{} {
	output := []interface{}{}
	textContent := []interface{}{}
	for _, item := range resp.Content {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			textContent = append(textContent, map[string]interface{}{
				"type":        "output_text",
				"text":        block["text"],
				"annotations": []interface{}{},
			})
		case "tool_use":
			arguments, _ := json.Marshal(block["input"])
			output = append(output, map[string]interface{}{
				"id":        block["id"],
				"type":      "function_call",
				"call_id":   block["id"],
				"name":      block["name"],
				"arguments": string(arguments),
				"status":    "completed",
			})
		}
	}
	if len(textContent) > 0 {
		output = append([]interface{}{map[string]interface{}{
			"id":      generateMessageID(),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": textContent,
		}}, output...)
	}
	return map[string]interface{}{
		"id":     resp.ID,
		"object": "response",
		"status": "completed",
		"model":  resp.Model,
		"output": output,
		"usage": map[string]interface{}{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
			"total_tokens":  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

func convertAnthropicToGoogleResponse(resp *MessagesResponse) map[string]interface{} {
	parts := []interface{}{}
	for _, item := range resp.Content {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			parts = append(parts, map[string]interface{}{"text": block["text"]})
		case "tool_use":
			parts = append(parts, map[string]interface{}{"functionCall": map[string]interface{}{"name": block["name"], "args": block["input"]}})
		}
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{"text": ""})
	}
	return map[string]interface{}{
		"candidates": []interface{}{map[string]interface{}{
			"content": map[string]interface{}{
				"role":  "model",
				"parts": parts,
			},
			"finishReason": anthropicStopReasonToGoogle(resp.StopReason),
		}},
		"usageMetadata": map[string]interface{}{
			"promptTokenCount":     resp.Usage.InputTokens,
			"candidatesTokenCount": resp.Usage.OutputTokens,
			"totalTokenCount":      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

func anthropicContentToPlainText(content []interface{}) string {
	var text strings.Builder
	for _, item := range content {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if block["type"] == "text" {
			if value, ok := block["text"].(string); ok {
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(value)
			}
		}
	}
	return text.String()
}

func anthropicContentToOpenAIToolCalls(content []interface{}) []interface{} {
	result := []interface{}{}
	for _, item := range content {
		block, ok := item.(map[string]interface{})
		if !ok || block["type"] != "tool_use" {
			continue
		}
		arguments, _ := json.Marshal(block["input"])
		result = append(result, map[string]interface{}{
			"id":   block["id"],
			"type": "function",
			"function": map[string]interface{}{
				"name":      block["name"],
				"arguments": string(arguments),
			},
		})
	}
	return result
}

func anthropicStopReasonToOpenAI(stopReason *string) string {
	if stopReason == nil {
		return "stop"
	}
	switch *stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func anthropicStopReasonToGoogle(stopReason *string) string {
	if stopReason == nil {
		return "STOP"
	}
	switch *stopReason {
	case "max_tokens":
		return "MAX_TOKENS"
	case "tool_use":
		return "TOOL_CALL"
	default:
		return "STOP"
	}
}

func HandleProtocolStreamProxy(ctx context.Context, w http.ResponseWriter, target ResolvedTarget, proxyURL string, originalHeaders http.Header, body []byte, pathType string, modelID string) (*StreamResult, error) {
	endpoint := buildProviderEndpoint(target, pathType, modelID, true)
	resp, err := CallProviderStream(ctx, target.API, body, proxyURL, endpoint, originalHeaders)
	if err != nil {
		http.Error(w, fmt.Sprintf("API error: %v", err), http.StatusInternalServerError)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("stream API error (status %d): %s", resp.StatusCode, string(respBody))
		http.Error(w, err.Error(), resp.StatusCode)
		return &StreamResult{StatusCode: resp.StatusCode, ResponseBody: formatLogBody(respBody)}, err
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil, fmt.Errorf("streaming not supported")
	}

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

	result := &StreamResult{StatusCode: resp.StatusCode}
	if _, err := io.Copy(flushWriter{Writer: w, Flusher: flusher}, resp.Body); err != nil {
		return result, err
	}
	return result, nil
}

type flushWriter struct {
	Writer  io.Writer
	Flusher http.Flusher
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil {
		w.Flusher.Flush()
	}
	return n, err
}
