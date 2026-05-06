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

func generateMessageID() string {
	return fmt.Sprintf("msg_%s", uuid.New().String()[:24])
}

func generateToolID() string {
	return fmt.Sprintf("toolu_%s", uuid.New().String()[:24])
}

// HandleOpenAIStream converts an OpenAI-compatible SSE stream to Anthropic SSE events.
func HandleOpenAIStream(ctx context.Context, w http.ResponseWriter, originalReq *MessagesRequest, openAIReq *OpenAIRequest, api config.APIConfig, proxyURL string, log *logger.Logger, routeID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	messageID := generateMessageID()

	// message_start
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

	// content_block_start for text
	writeSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "",
		},
	})

	// ping
	writeSSE(w, flusher, "ping", map[string]interface{}{"type": "ping"})

	// Prepare request body
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
		finalizeStream(w, flusher, 0, false, 0)
		return
	}

	endpoint := api.BaseURL + "/chat/completions"
	resp, err := CallProviderStream(ctx, api, jsonBody, proxyURL, endpoint)
	if err != nil {
		slog.Error("stream request failed", slog.String("error", err.Error()))
		finalizeStream(w, flusher, 0, false, 0)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("stream API error", slog.Int("status", resp.StatusCode), slog.String("body", string(body)))
		finalizeStream(w, flusher, 0, false, 0)
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var mu sync.Mutex
	toolIndex := -1
	lastToolIndex := 0
	textBlockClosed := false
	outputTokens := 0
	hasSentStopReason := false

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
			outputTokens = chunk.Usage.CompletionTokens
		}

		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			delta := choice.Delta

			if delta.Content != "" && toolIndex < 0 && !textBlockClosed {
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
				if toolIndex < 0 {
					if !textBlockClosed {
						textBlockClosed = true
						writeSSE(w, flusher, "content_block_stop", map[string]interface{}{
							"type":  "content_block_stop",
							"index": 0,
						})
					}
				}

				for _, toolCall := range delta.ToolCalls {
					if toolCall.ID != "" {
						lastToolIndex++
						toolIndex = lastToolIndex

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
					writeSSE(w, flusher, "content_block_stop", map[string]interface{}{
						"type":  "content_block_stop",
						"index": i,
					})
				}

				if !textBlockClosed {
					writeSSE(w, flusher, "content_block_stop", map[string]interface{}{
						"type":  "content_block_stop",
						"index": 0,
					})
				}

				stopReason := "end_turn"
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
					"usage": map[string]int{
						"output_tokens": outputTokens,
					},
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
	}

	if !hasSentStopReason {
		finalizeStream(w, flusher, lastToolIndex, textBlockClosed, outputTokens)
	}
}

// HandleAnthropicStreamProxy pipes the Anthropic SSE stream directly to the client.
func HandleAnthropicStreamProxy(ctx context.Context, w http.ResponseWriter, api config.APIConfig, proxyURL string, body []byte, suffix string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	endpoint := BuildEndpoint(api.BaseURL, suffix)
	resp, err := CallProviderStream(ctx, api, body, proxyURL, endpoint)
	if err != nil {
		slog.Error("failed to call Anthropic API", slog.String("error", err.Error()))
		http.Error(w, fmt.Sprintf("API error: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if reqID := resp.Header.Get("x-request-id"); reqID != "" {
		w.Header().Set("x-request-id", reqID)
	}

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		slog.Debug("stream line", slog.String("line", line))
		fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		slog.Error("failed to read stream", slog.String("error", err.Error()))
	}
}

func finalizeStream(w http.ResponseWriter, flusher http.Flusher, lastToolIndex int, textBlockClosed bool, outputTokens int) {
	for i := 1; i <= lastToolIndex; i++ {
		writeSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": i,
		})
	}

	if !textBlockClosed {
		writeSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})
	}

	writeSSE(w, flusher, "message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]int{
			"output_tokens": outputTokens,
		},
	})

	writeSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}
