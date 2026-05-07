package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/mark0725/go-agents-proxy/internal/config"
	"github.com/mark0725/go-agents-proxy/internal/logger"
)

type StreamResult struct {
	StatusCode   int
	InputTokens  int
	OutputTokens int
	StopReason   string
	ResponseBody string
}

func generateMessageID() string {
	return fmt.Sprintf("msg_%s", uuid.New().String()[:24])
}

func generateToolID() string {
	return fmt.Sprintf("toolu_%s", uuid.New().String()[:24])
}

// HandleOpenAIStream converts an OpenAI-compatible SSE stream to Anthropic SSE events.
func HandleOpenAIStream(ctx context.Context, w http.ResponseWriter, originalReq *MessagesRequest, openAIReq *OpenAIRequest, api config.APIConfig, proxyURL string, originalHeaders http.Header, log *logger.Logger, routeID string) (*StreamResult, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil, fmt.Errorf("streaming not supported")
	}

	openAIReq.Stream = true
	for i := range openAIReq.Messages {
		if content, ok := openAIReq.Messages[i].Content.([]map[string]interface{}); ok {
			var textContent strings.Builder
			for _, block := range content {
				switch block["type"] {
				case "text":
					if text, ok := block["text"].(string); ok {
						textContent.WriteString(text + "\n")
					}
				case "tool_result":
					toolID := ""
					if id, ok := block["tool_use_id"].(string); ok {
						toolID = id
					}
					textContent.WriteString(fmt.Sprintf("[Tool Result ID: %s]\n", toolID))
					textContent.WriteString(parseToolResultContent(block["content"]) + "\n")
				}
			}
			if textContent.Len() == 0 {
				openAIReq.Messages[i].Content = "..."
			} else {
				openAIReq.Messages[i].Content = strings.TrimSpace(textContent.String())
			}
		}
		if openAIReq.Messages[i].Content == nil {
			openAIReq.Messages[i].Content = "..."
		}
	}

	jsonBody, err := json.Marshal(openAIReq)
	if err != nil {
		slog.Error("failed to marshal stream request", slog.String("error", err.Error()))
		return nil, err
	}

	endpoint := api.BaseURL + "/chat/completions"
	resp, err := CallProviderStream(ctx, api, jsonBody, proxyURL, endpoint, originalHeaders)
	if err != nil {
		slog.Error("stream request failed", slog.String("error", err.Error()))
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("stream API error (status %d): %s", resp.StatusCode, string(body))
		slog.Error("stream API error", slog.Int("status", resp.StatusCode), slog.String("body", string(body)))
		return &StreamResult{StatusCode: resp.StatusCode, ResponseBody: formatLogBody(body)}, err
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	messageID := generateMessageID()
	writeSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         originalReq.Model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":                0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"output_tokens":               0,
			},
		},
	})
	writeSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "",
		},
	})
	writeSSE(w, flusher, "ping", map[string]interface{}{"type": "ping"})

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var mu sync.Mutex
	toolIndex := -1
	lastToolIndex := 0
	textBlockClosed := false
	inputTokens := 0
	outputTokens := 0
	hasSentStopReason := false
	stopReason := ""
	var responseText strings.Builder
	toolUses := make([]map[string]interface{}, 0)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		mu.Lock()
		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			delta := choice.Delta
			if delta.Content != "" && toolIndex < 0 && !textBlockClosed {
				responseText.WriteString(delta.Content)
				writeSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]interface{}{
						"type": "text_delta",
						"text": delta.Content,
					},
				})
			}
			if len(delta.ToolCalls) > 0 {
				if toolIndex < 0 && !textBlockClosed {
					textBlockClosed = true
					writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
				}
				for _, toolCall := range delta.ToolCalls {
					if toolCall.ID != "" {
						lastToolIndex++
						toolIndex = lastToolIndex
						toolUses = append(toolUses, map[string]interface{}{
							"id":   toolCall.ID,
							"name": toolCall.Function.Name,
						})
						writeSSE(w, flusher, "content_block_start", map[string]interface{}{
							"type":  "content_block_start",
							"index": toolIndex,
							"content_block": map[string]interface{}{
								"type":  "tool_use",
								"id":    toolCall.ID,
								"name":  toolCall.Function.Name,
								"input": map[string]interface{}{},
							},
						})
					}
					if toolCall.Function.Arguments != "" {
						if len(toolUses) > 0 {
							toolUses[len(toolUses)-1]["arguments"] = toolCall.Function.Arguments
						}
						writeSSE(w, flusher, "content_block_delta", map[string]interface{}{
							"type":  "content_block_delta",
							"index": toolIndex,
							"delta": map[string]interface{}{
								"type":         "input_json_delta",
								"partial_json": toolCall.Function.Arguments,
							},
						})
					}
				}
			}
			if choice.FinishReason != nil && !hasSentStopReason {
				hasSentStopReason = true
				for i := 1; i <= lastToolIndex; i++ {
					writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": i})
				}
				if !textBlockClosed {
					writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
				}
				stopReason = "end_turn"
				switch *choice.FinishReason {
				case "length":
					stopReason = "max_tokens"
				case "tool_calls":
					stopReason = "tool_use"
				}
				writeSSE(w, flusher, "message_delta", map[string]interface{}{
					"type": "message_delta",
					"delta": map[string]interface{}{
						"stop_reason":   stopReason,
						"stop_sequence": nil,
					},
					"usage": map[string]int{"output_tokens": outputTokens},
				})
				writeSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
			}
		}
		mu.Unlock()
	}

	if err := scanner.Err(); err != nil {
		slog.Error("stream read error", slog.String("error", err.Error()))
		writeSSE(w, flusher, "error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "stream_error", "message": "Upstream stream failed"},
		})
		return &StreamResult{
			StatusCode:   resp.StatusCode,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			ResponseBody: marshalLogValue(map[string]interface{}{"text": responseText.String(), "tool_uses": toolUses}),
		}, err
	}

	if !hasSentStopReason {
		writeSSE(w, flusher, "error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "stream_error", "message": "Upstream stream ended without stop reason"},
		})
		return &StreamResult{
			StatusCode:   resp.StatusCode,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			ResponseBody: marshalLogValue(map[string]interface{}{"text": responseText.String(), "tool_uses": toolUses}),
		}, fmt.Errorf("upstream stream ended without stop reason")
	}

	return &StreamResult{
		StatusCode:   resp.StatusCode,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		StopReason:   stopReason,
		ResponseBody: marshalLogValue(map[string]interface{}{"text": responseText.String(), "tool_uses": toolUses}),
	}, nil
}

// HandleAnthropicStreamProxy pipes the Anthropic SSE stream directly to the client.
func HandleAnthropicStreamProxy(ctx context.Context, w http.ResponseWriter, api config.APIConfig, proxyURL string, originalHeaders http.Header, body []byte, suffix string) (*StreamResult, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil, fmt.Errorf("streaming not supported")
	}

	endpoint := BuildEndpoint(api.BaseURL, suffix)
	resp, err := CallProviderStream(ctx, api, body, proxyURL, endpoint, originalHeaders)
	if err != nil {
		slog.Error("failed to call Anthropic API", slog.String("error", err.Error()))
		http.Error(w, fmt.Sprintf("API error: %v", err), http.StatusInternalServerError)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("anthropic stream API error (status %d): %s", resp.StatusCode, string(body))
		http.Error(w, err.Error(), resp.StatusCode)
		return &StreamResult{StatusCode: resp.StatusCode, ResponseBody: formatLogBody(body)}, err
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if reqID := resp.Header.Get("x-request-id"); reqID != "" {
		w.Header().Set("x-request-id", reqID)
	}

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	result := &StreamResult{StatusCode: resp.StatusCode}
	for scanner.Scan() {
		line := scanner.Text()
		slog.Debug("stream line", slog.String("line", line))
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data != "[DONE]" {
				var event struct {
					Type    string `json:"type"`
					Usage   *Usage `json:"usage,omitempty"`
					Message *struct {
						Usage *Usage `json:"usage,omitempty"`
					} `json:"message,omitempty"`
					Delta struct {
						StopReason string `json:"stop_reason,omitempty"`
					} `json:"delta,omitempty"`
					Error map[string]string `json:"error,omitempty"`
				}
				if err := json.Unmarshal([]byte(data), &event); err == nil {
					if event.Usage != nil {
						result.InputTokens = event.Usage.InputTokens
						result.OutputTokens = event.Usage.OutputTokens
					}
					if event.Message != nil && event.Message.Usage != nil {
						if result.InputTokens == 0 {
							result.InputTokens = event.Message.Usage.InputTokens
						}
						if result.OutputTokens == 0 {
							result.OutputTokens = event.Message.Usage.OutputTokens
						}
					}
					if event.Delta.StopReason != "" {
						result.StopReason = event.Delta.StopReason
					}
					if len(event.Error) > 0 {
						result.ResponseBody = marshalLogValue(event)
					}
				}
			}
		}
		fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		slog.Error("failed to read stream", slog.String("error", err.Error()))
		return result, err
	}
	if result.ResponseBody == "" {
		result.ResponseBody = marshalLogValue(map[string]interface{}{"stop_reason": result.StopReason})
	}
	return result, nil
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}
