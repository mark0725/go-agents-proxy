package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================================
// Anthropic Data Models
// ============================================================================

type ContentBlockText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ContentBlockImage struct {
	Type   string                 `json:"type"`
	Source map[string]interface{} `json:"source"`
}

type ContentBlockToolUse struct {
	Type  string                 `json:"type"`
	ID    string                 `json:"id"`
	Name  string                 `json:"name"`
	Input map[string]interface{} `json:"input"`
}

type ContentBlockToolResult struct {
	Type      string      `json:"type"`
	ToolUseID string      `json:"tool_use_id"`
	Content   interface{} `json:"content"`
}

type SystemContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type MessagesRequest struct {
	Model         string                 `json:"model"`
	MaxTokens     int                    `json:"max_tokens"`
	Messages      []Message              `json:"messages"`
	System        interface{}            `json:"system,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	TopK          *int                   `json:"top_k,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
	Tools         []Tool                 `json:"tools,omitempty"`
	ToolChoice    map[string]interface{} `json:"tool_choice,omitempty"`
	Thinking      *ThinkingConfig        `json:"thinking,omitempty"`
}

type TokenCountRequest struct {
	Model      string                 `json:"model"`
	Messages   []Message              `json:"messages"`
	System     interface{}            `json:"system,omitempty"`
	Tools      []Tool                 `json:"tools,omitempty"`
	Thinking   *ThinkingConfig        `json:"thinking,omitempty"`
	ToolChoice map[string]interface{} `json:"tool_choice,omitempty"`
}

type TokenCountResponse struct {
	InputTokens int `json:"input_tokens"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type MessagesResponse struct {
	ID           string        `json:"id"`
	Model        string        `json:"model"`
	Role         string        `json:"role"`
	Content      []interface{} `json:"content"`
	Type         string        `json:"type"`
	StopReason   *string       `json:"stop_reason"`
	StopSequence *string       `json:"stop_sequence"`
	Usage        Usage         `json:"usage"`
}

// ============================================================================
// OpenAI Data Models
// ============================================================================

type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type OpenAIRequest struct {
	Model               string          `json:"model"`
	Messages            []OpenAIMessage `json:"messages"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	Stop                []string        `json:"stop,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Tools               []OpenAITool    `json:"tools,omitempty"`
	ToolChoice          interface{}     `json:"tool_choice,omitempty"`
}

type OpenAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int           `json:"index"`
		Message      OpenAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type OpenAIStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string           `json:"role,omitempty"`
			Content   string           `json:"content,omitempty"`
			ToolCalls []OpenAIToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

// ============================================================================
// Error Response
// ============================================================================

type ErrorResponse struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ============================================================================
// Provider type
// ============================================================================

type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderGoogle    Provider = "google"
	ProviderAnthropic Provider = "anthropic"
)

// ============================================================================
// Helper Functions
// ============================================================================

func cleanGoogleSchema(schema interface{}) interface{} {
	switch s := schema.(type) {
	case map[string]interface{}:
		delete(s, "additionalProperties")
		delete(s, "default")

		if t, ok := s["type"].(string); ok && t == "string" {
			if format, ok := s["format"].(string); ok {
				allowedFormats := map[string]bool{"enum": true, "date-time": true}
				if !allowedFormats[format] {
					delete(s, "format")
				}
			}
		}

		for key, value := range s {
			s[key] = cleanGoogleSchema(value)
		}
		return s

	case []interface{}:
		result := make([]interface{}, len(s))
		for i, item := range s {
			result[i] = cleanGoogleSchema(item)
		}
		return result

	default:
		return schema
	}
}

func parseToolResultContent(content interface{}) string {
	if content == nil {
		return "No content provided"
	}

	switch c := content.(type) {
	case string:
		return c

	case []interface{}:
		var result strings.Builder
		for _, item := range c {
			switch i := item.(type) {
			case map[string]interface{}:
				if i["type"] == "text" {
					if text, ok := i["text"].(string); ok {
						result.WriteString(text + "\n")
					}
				} else if text, ok := i["text"].(string); ok {
					result.WriteString(text + "\n")
				} else {
					if jsonBytes, err := json.Marshal(i); err == nil {
						result.WriteString(string(jsonBytes) + "\n")
					}
				}
			case string:
				result.WriteString(i + "\n")
			default:
				result.WriteString(fmt.Sprintf("%v\n", i))
			}
		}
		return strings.TrimSpace(result.String())

	case map[string]interface{}:
		if c["type"] == "text" {
			if text, ok := c["text"].(string); ok {
				return text
			}
		}
		if jsonBytes, err := json.Marshal(c); err == nil {
			return string(jsonBytes)
		}
		return fmt.Sprintf("%v", c)

	default:
		return fmt.Sprintf("%v", c)
	}
}

// ============================================================================
// Conversion Functions
// ============================================================================

func ConvertAnthropicToOpenAI(req *MessagesRequest, provider Provider) (*OpenAIRequest, error) {
	messages := []OpenAIMessage{}

	if req.System != nil {
		switch s := req.System.(type) {
		case string:
			messages = append(messages, OpenAIMessage{Role: "system", Content: s})
		case []interface{}:
			var systemText strings.Builder
			for _, block := range s {
				if blockMap, ok := block.(map[string]interface{}); ok {
					if blockMap["type"] == "text" {
						if text, ok := blockMap["text"].(string); ok {
							systemText.WriteString(text + "\n\n")
						}
					}
				}
			}
			if systemText.Len() > 0 {
				messages = append(messages, OpenAIMessage{Role: "system", Content: strings.TrimSpace(systemText.String())})
			}
		}
	}

	for _, msg := range req.Messages {
		switch content := msg.Content.(type) {
		case string:
			messages = append(messages, OpenAIMessage{Role: msg.Role, Content: content})

		case []interface{}:
			hasToolResult := false
			for _, block := range content {
				if blockMap, ok := block.(map[string]interface{}); ok {
					if blockMap["type"] == "tool_result" {
						hasToolResult = true
						break
					}
				}
			}

			if msg.Role == "user" && hasToolResult {
				var textContent strings.Builder
				for _, block := range content {
					if blockMap, ok := block.(map[string]interface{}); ok {
						switch blockMap["type"] {
						case "text":
							if text, ok := blockMap["text"].(string); ok {
								textContent.WriteString(text + "\n")
							}
						case "tool_result":
							toolID := ""
							if id, ok := blockMap["tool_use_id"].(string); ok {
								toolID = id
							}
							resultContent := parseToolResultContent(blockMap["content"])
							textContent.WriteString(fmt.Sprintf("Tool result for %s:\n%s\n", toolID, resultContent))
						}
					}
				}
				messages = append(messages, OpenAIMessage{Role: "user", Content: strings.TrimSpace(textContent.String())})
			} else {
				var processedContent []map[string]interface{}
				for _, block := range content {
					if blockMap, ok := block.(map[string]interface{}); ok {
						switch blockMap["type"] {
						case "text":
							processedContent = append(processedContent, map[string]interface{}{
								"type": "text",
								"text": blockMap["text"],
							})
						case "image":
							processedContent = append(processedContent, map[string]interface{}{
								"type":   "image",
								"source": blockMap["source"],
							})
						case "tool_use":
							processedContent = append(processedContent, map[string]interface{}{
								"type":  "tool_use",
								"id":    blockMap["id"],
								"name":  blockMap["name"],
								"input": blockMap["input"],
							})
						}
					}
				}
				messages = append(messages, OpenAIMessage{Role: msg.Role, Content: processedContent})
			}
		}
	}

	maxTokens := req.MaxTokens
	if provider == ProviderOpenAI || provider == ProviderGoogle {
		if maxTokens > 16384 {
			maxTokens = 16384
		}
	}

	openAIReq := &OpenAIRequest{
		Model:               req.Model,
		Messages:            messages,
		MaxCompletionTokens: maxTokens,
		Temperature:         req.Temperature,
		Stream:              req.Stream,
	}

	if len(req.StopSequences) > 0 {
		openAIReq.Stop = req.StopSequences
	}

	if req.TopP != nil {
		openAIReq.TopP = req.TopP
	}

	if len(req.Tools) > 0 {
		isGoogleModel := provider == ProviderGoogle
		for _, tool := range req.Tools {
			inputSchema := tool.InputSchema
			if isGoogleModel {
				inputSchema = cleanGoogleSchema(inputSchema).(map[string]interface{})
			}

			openAIReq.Tools = append(openAIReq.Tools, OpenAITool{
				Type: "function",
				Function: OpenAIFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  inputSchema,
				},
			})
		}
	}

	if req.ToolChoice != nil {
		choiceType, _ := req.ToolChoice["type"].(string)
		switch choiceType {
		case "auto":
			openAIReq.ToolChoice = "auto"
		case "any":
			openAIReq.ToolChoice = "any"
		case "tool":
			if name, ok := req.ToolChoice["name"].(string); ok {
				openAIReq.ToolChoice = map[string]interface{}{
					"type": "function",
					"function": map[string]string{
						"name": name,
					},
				}
			}
		default:
			openAIReq.ToolChoice = "auto"
		}
	}

	return openAIReq, nil
}

func ConvertOpenAIToAnthropic(openAIResp *OpenAIResponse, originalReq *MessagesRequest, provider Provider) *MessagesResponse {
	content := []interface{}{}

	if len(openAIResp.Choices) > 0 {
		choice := openAIResp.Choices[0]
		message := choice.Message

		if contentStr, ok := message.Content.(string); ok && contentStr != "" {
			content = append(content, map[string]interface{}{
				"type": "text",
				"text": contentStr,
			})
		}

		isAnthropicProvider := provider == ProviderAnthropic

		if len(message.ToolCalls) > 0 && isAnthropicProvider {
			for _, toolCall := range message.ToolCalls {
				var args map[string]interface{}
				json.Unmarshal([]byte(toolCall.Function.Arguments), &args)
				if args == nil {
					args = map[string]interface{}{"raw": toolCall.Function.Arguments}
				}

				content = append(content, map[string]interface{}{
					"type":  "tool_use",
					"id":    toolCall.ID,
					"name":  toolCall.Function.Name,
					"input": args,
				})
			}
		} else if len(message.ToolCalls) > 0 && !isAnthropicProvider {
			var toolText strings.Builder
			toolText.WriteString("\n\nTool usage:\n")
			for _, toolCall := range message.ToolCalls {
				toolText.WriteString(fmt.Sprintf("Tool: %s\nArguments: %s\n\n", toolCall.Function.Name, toolCall.Function.Arguments))
			}

			if len(content) > 0 {
				if textBlock, ok := content[0].(map[string]interface{}); ok {
					if textBlock["type"] == "text" {
						textBlock["text"] = textBlock["text"].(string) + toolText.String()
					}
				}
			} else {
				content = append(content, map[string]interface{}{
					"type": "text",
					"text": toolText.String(),
				})
			}
		}
	}

	if len(content) == 0 {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": "",
		})
	}

	var stopReason *string
	if len(openAIResp.Choices) > 0 {
		finishReason := openAIResp.Choices[0].FinishReason
		var reason string
		switch finishReason {
		case "stop":
			reason = "end_turn"
		case "length":
			reason = "max_tokens"
		case "tool_calls":
			reason = "tool_use"
		default:
			reason = "end_turn"
		}
		stopReason = &reason
	}

	return &MessagesResponse{
		ID:         openAIResp.ID,
		Model:      originalReq.Model,
		Role:       "assistant",
		Content:    content,
		Type:       "message",
		StopReason: stopReason,
		Usage: Usage{
			InputTokens:  openAIResp.Usage.PromptTokens,
			OutputTokens: openAIResp.Usage.CompletionTokens,
		},
	}
}
