package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

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

type sseEvent struct {
	Event string
	Data  string
	Raw   []byte
}

type openAIStreamEvent struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role,omitempty"`
			Content   string `json:"content,omitempty"`
			ToolCalls []struct {
				Index    *int   `json:"index,omitempty"`
				ID       string `json:"id,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

type openAIToolStreamState struct {
	ContentIndex int
	ID           string
	Name         string
	Arguments    strings.Builder
	Started      bool
}

func generateMessageID() string {
	return fmt.Sprintf("msg_%s", uuid.New().String()[:24])
}

func generateToolID() string {
	return fmt.Sprintf("toolu_%s", uuid.New().String()[:24])
}

// HandleOpenAIStream converts an OpenAI-compatible SSE stream to Anthropic SSE events.
func HandleOpenAIStream(ctx context.Context, w http.ResponseWriter, originalReq *MessagesRequest, openAIReq *OpenAIRequest, api config.APIConfig, proxyURL string, originalHeaders http.Header, log *logger.Logger, routeID string) (*StreamResult, error) {
	_ = log
	_ = routeID

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil, fmt.Errorf("streaming not supported")
	}

	openAIReq.Stream = true
	jsonBody, err := json.Marshal(openAIReq)
	if err != nil {
		slog.Error("failed to marshal stream request", slog.String("error", err.Error()))
		return nil, err
	}

	endpoint := BuildEndpoint(api.BaseURL, "/chat/completions")
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

	prepareStreamResponseHeaders(w, resp.Header)
	writeAnthropicMessageStart(w, flusher, originalReq.Model)

	reader := bufio.NewReader(resp.Body)
	result := &StreamResult{StatusCode: resp.StatusCode}
	states := map[int]*openAIToolStreamState{}
	toolOrder := make([]int, 0)
	textBlockClosed := false
	stopReasonSent := false
	var responseText strings.Builder

	for {
		event, err := readSSEEvent(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			writeSSE(w, flusher, "error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "stream_error", "message": "Upstream stream failed"},
			})
			result.ResponseBody = marshalLogValue(map[string]interface{}{"text": responseText.String(), "tool_uses": buildToolLog(states, toolOrder)})
			return result, err
		}

		if event == nil || event.Data == "" {
			continue
		}
		if event.Data == "[DONE]" {
			break
		}

		var chunk openAIStreamEvent
		if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
			continue
		}

		if chunk.Usage != nil {
			result.InputTokens = chunk.Usage.PromptTokens
			result.OutputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta
		if delta.Content != "" && !textBlockClosed {
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
			if !textBlockClosed {
				textBlockClosed = true
				writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
			}
			for i, toolCall := range delta.ToolCalls {
				stateKey := i
				if toolCall.Index != nil {
					stateKey = *toolCall.Index
				}
				state, exists := states[stateKey]
				if !exists {
					state = &openAIToolStreamState{ContentIndex: len(states) + 1}
					states[stateKey] = state
					toolOrder = append(toolOrder, stateKey)
				}
				if toolCall.ID != "" {
					state.ID = toolCall.ID
				}
				if toolCall.Function.Name != "" {
					state.Name = toolCall.Function.Name
				}
				if toolCall.Function.Arguments != "" {
					state.Arguments.WriteString(toolCall.Function.Arguments)
				}
				if !state.Started && state.ID != "" && state.Name != "" {
					state.Started = true
					writeSSE(w, flusher, "content_block_start", map[string]interface{}{
						"type":  "content_block_start",
						"index": state.ContentIndex,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    state.ID,
							"name":  state.Name,
							"input": map[string]interface{}{},
						},
					})
					if state.Arguments.Len() > 0 {
						writeSSE(w, flusher, "content_block_delta", map[string]interface{}{
							"type":  "content_block_delta",
							"index": state.ContentIndex,
							"delta": map[string]interface{}{
								"type":         "input_json_delta",
								"partial_json": state.Arguments.String(),
							},
						})
					}
				} else if state.Started && toolCall.Function.Arguments != "" {
					writeSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": state.ContentIndex,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": toolCall.Function.Arguments,
						},
					})
				}
			}
		}

		if choice.FinishReason != nil && !stopReasonSent {
			stopReasonSent = true
			rescueIncompleteToolStates(w, flusher, states, toolOrder)
			for _, key := range toolOrder {
				state := states[key]
				if state != nil && state.Started {
					writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": state.ContentIndex})
				}
			}
			if !textBlockClosed {
				writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
			}
			result.StopReason = normalizeOpenAIFinishReason(*choice.FinishReason)
			writeSSE(w, flusher, "message_delta", map[string]interface{}{
				"type": "message_delta",
				"delta": map[string]interface{}{
					"stop_reason":   result.StopReason,
					"stop_sequence": nil,
				},
				"usage": map[string]int{"output_tokens": result.OutputTokens},
			})
			writeSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
	}

	result.ResponseBody = marshalLogValue(map[string]interface{}{"text": responseText.String(), "tool_uses": buildToolLog(states, toolOrder)})
	if !stopReasonSent {
		writeSSE(w, flusher, "error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "stream_error", "message": "Upstream stream ended without stop reason"},
		})
		return result, fmt.Errorf("upstream stream ended without stop reason")
	}
	return result, nil
}

// HandleAnthropicStreamProxy pipes the Anthropic SSE stream directly to the client,
// performing event-level parsing so the streaming log record gets real usage,
// stop_reason, and upstream error info.
func HandleAnthropicStreamProxy(ctx context.Context, w http.ResponseWriter, target ResolvedTarget, proxyURL string, originalHeaders http.Header, body []byte) (*StreamResult, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil, fmt.Errorf("streaming not supported")
	}

	endpoint := buildProviderEndpoint(target, "messages", target.ModelID, true)
	resp, err := CallProviderStream(ctx, target.API, body, proxyURL, endpoint, originalHeaders)
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

	prepareStreamResponseHeaders(w, resp.Header)
	reader := bufio.NewReader(resp.Body)
	result := &StreamResult{StatusCode: resp.StatusCode}
	for {
		event, err := readSSEEvent(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			slog.Error("failed to read stream", slog.String("error", err.Error()))
			return result, err
		}
		if event == nil {
			continue
		}
		if len(event.Raw) > 0 {
			if _, err := w.Write(event.Raw); err != nil {
				return result, err
			}
			flusher.Flush()
		}
		if event.Data == "" || event.Data == "[DONE]" {
			continue
		}

		var parsed struct {
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
		if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
			continue
		}
		if parsed.Usage != nil {
			result.InputTokens = parsed.Usage.InputTokens
			result.OutputTokens = parsed.Usage.OutputTokens
		}
		if parsed.Message != nil && parsed.Message.Usage != nil {
			if result.InputTokens == 0 {
				result.InputTokens = parsed.Message.Usage.InputTokens
			}
			if result.OutputTokens == 0 {
				result.OutputTokens = parsed.Message.Usage.OutputTokens
			}
		}
		if parsed.Delta.StopReason != "" {
			result.StopReason = parsed.Delta.StopReason
		}
		if len(parsed.Error) > 0 {
			result.ResponseBody = marshalLogValue(parsed)
		}
	}
	if result.ResponseBody == "" {
		result.ResponseBody = marshalLogValue(map[string]interface{}{"stop_reason": result.StopReason})
	}
	return result, nil
}

func HandleGoogleStreamAsAnthropic(ctx context.Context, w http.ResponseWriter, originalReq *MessagesRequest, target ResolvedTarget, proxyURL string, originalHeaders http.Header, body []byte) (*StreamResult, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return nil, fmt.Errorf("streaming not supported")
	}

	endpoint := buildProviderEndpoint(target, "messages", target.ModelID, true)
	resp, err := CallProviderStream(ctx, target.API, body, proxyURL, endpoint, originalHeaders)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("google stream API error (status %d): %s", resp.StatusCode, string(respBody))
		return &StreamResult{StatusCode: resp.StatusCode, ResponseBody: formatLogBody(respBody)}, err
	}

	prepareStreamResponseHeaders(w, resp.Header)
	writeAnthropicMessageStart(w, flusher, originalReq.Model)

	reader := bufio.NewReader(resp.Body)
	result := &StreamResult{StatusCode: resp.StatusCode}
	stopReason := "end_turn"
	hasText := false
	textBlockClosed := false
	nextToolIndex := 1
	toolUses := make([]map[string]interface{}, 0)
	var responseText strings.Builder

	for {
		event, err := readSSEEvent(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return result, err
		}
		if event == nil || event.Data == "" || event.Data == "[DONE]" {
			continue
		}

		var parsed struct {
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
		if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
			continue
		}
		if parsed.UsageMetadata.PromptTokenCount > 0 {
			result.InputTokens = parsed.UsageMetadata.PromptTokenCount
		}
		if parsed.UsageMetadata.CandidatesTokenCount > 0 {
			result.OutputTokens = parsed.UsageMetadata.CandidatesTokenCount
		}

		for _, candidate := range parsed.Candidates {
			for _, part := range candidate.Content.Parts {
				if text, ok := part["text"].(string); ok && text != "" {
					hasText = true
					responseText.WriteString(text)
					writeSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": 0,
						"delta": map[string]interface{}{"type": "text_delta", "text": text},
					})
				}
				if functionCall, ok := part["functionCall"].(map[string]interface{}); ok {
					if !textBlockClosed {
						textBlockClosed = true
						writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
					}
					toolID := generateToolID()
					name, _ := functionCall["name"].(string)
					args := map[string]interface{}{}
					if value, ok := functionCall["args"].(map[string]interface{}); ok {
						args = value
					}
					writeSSE(w, flusher, "content_block_start", map[string]interface{}{
						"type":          "content_block_start",
						"index":         nextToolIndex,
						"content_block": map[string]interface{}{"type": "tool_use", "id": toolID, "name": name, "input": map[string]interface{}{}},
					})
					arguments, _ := json.Marshal(args)
					writeSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": nextToolIndex,
						"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(arguments)},
					})
					writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": nextToolIndex})
					toolUses = append(toolUses, map[string]interface{}{"id": toolID, "name": name, "arguments": string(arguments)})
					nextToolIndex++
					stopReason = "tool_use"
				}
			}
			switch candidate.FinishReason {
			case "MAX_TOKENS":
				stopReason = "max_tokens"
			case "TOOL_CALL", "MALFORMED_FUNCTION_CALL":
				stopReason = "tool_use"
			case "STOP":
				if stopReason == "" {
					stopReason = "end_turn"
				}
			}
		}
	}

	if !textBlockClosed {
		if !hasText {
			writeSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]interface{}{"type": "text_delta", "text": ""},
			})
		}
		writeSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	}
	result.StopReason = stopReason
	writeSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": result.OutputTokens},
	})
	writeSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
	result.ResponseBody = marshalLogValue(map[string]interface{}{"text": responseText.String(), "tool_uses": toolUses, "stop_reason": stopReason})
	return result, nil
}

func prepareStreamResponseHeaders(w http.ResponseWriter, headers http.Header) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	copyNormalizedResponseHeaders(w.Header(), headers)
}

func writeAnthropicMessageStart(w http.ResponseWriter, flusher http.Flusher, model string) {
	messageID := generateMessageID()
	writeSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
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
}

func readSSEEvent(reader *bufio.Reader) (*sseEvent, error) {
	var raw bytes.Buffer
	var dataLines []string
	event := &sseEvent{}
	readAny := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && readAny {
				raw.WriteString(line)
				trimmed := strings.TrimRight(line, "\r\n")
				if strings.HasPrefix(trimmed, "data:") {
					dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
				} else if strings.HasPrefix(trimmed, "event:") {
					event.Event = strings.TrimPrefix(strings.TrimPrefix(trimmed, "event:"), " ")
				}
				event.Data = strings.Join(dataLines, "\n")
				event.Raw = raw.Bytes()
				return event, nil
			}
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		readAny = true
		raw.WriteString(line)
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			event.Data = strings.Join(dataLines, "\n")
			event.Raw = raw.Bytes()
			return event, nil
		}
		if strings.HasPrefix(trimmed, ":") {
			continue
		}
		if strings.HasPrefix(trimmed, "event:") {
			event.Event = strings.TrimPrefix(strings.TrimPrefix(trimmed, "event:"), " ")
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
		}
	}
}

func buildToolLog(states map[int]*openAIToolStreamState, order []int) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(order))
	for _, key := range order {
		state := states[key]
		if state == nil {
			continue
		}
		result = append(result, map[string]interface{}{
			"id":        state.ID,
			"name":      state.Name,
			"arguments": state.Arguments.String(),
		})
	}
	return result
}

// rescueIncompleteToolStates emits any tool_use blocks whose state accumulated arguments
// but never received both an ID and a Name in time to open a block during streaming.
// Without this rescue the buffered arguments would be silently dropped at finish_reason.
func rescueIncompleteToolStates(w http.ResponseWriter, flusher http.Flusher, states map[int]*openAIToolStreamState, order []int) {
	for _, key := range order {
		state := states[key]
		if state == nil || state.Started || state.Arguments.Len() == 0 {
			continue
		}
		if state.ID == "" {
			state.ID = generateToolID()
		}
		if state.Name == "" {
			state.Name = "unknown"
		}
		state.Started = true
		writeSSE(w, flusher, "content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": state.ContentIndex,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    state.ID,
				"name":  state.Name,
				"input": map[string]interface{}{},
			},
		})
		writeSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": state.ContentIndex,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": state.Arguments.String(),
			},
		})
		slog.Warn("stream: rescued tool_call with missing id/name at finish",
			slog.String("id", state.ID),
			slog.String("name", state.Name),
			slog.Int("args_len", state.Arguments.Len()),
		)
	}
}

func normalizeOpenAIFinishReason(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}
