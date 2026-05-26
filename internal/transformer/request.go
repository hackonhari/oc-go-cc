// Package transformer handles request and response format conversion
// between Anthropic Messages API and OpenAI Chat Completions API.
package transformer

import (
	"encoding/json"
	"fmt"
	"strings"

	"oc-go-cc/internal/config"
	"oc-go-cc/pkg/types"
)

// RequestTransformer converts Anthropic requests to OpenAI format.
type RequestTransformer struct{}

// NewRequestTransformer creates a new request transformer.
func NewRequestTransformer() *RequestTransformer {
	return &RequestTransformer{}
}

// isDeepSeekModel returns true for DeepSeek models that require thinking mode handling.
func isDeepSeekModel(modelID string) bool {
	return strings.HasPrefix(modelID, "deepseek-")
}

// needsPlaceholderReasoning returns true for providers whose validators require
// a non-empty reasoning_content field on assistant tool-call messages.
func needsPlaceholderReasoning(modelID string) bool {
	// Moonshot's validator treats an empty string as missing.
	return strings.HasPrefix(modelID, "kimi-")
}

// TransformRequest converts an Anthropic MessageRequest to OpenAI ChatCompletionRequest.
func (t *RequestTransformer) TransformRequest(
	anthropicReq *types.MessageRequest,
	model config.ModelConfig,
) (*types.ChatCompletionRequest, error) {
	// Decide thinking mode FIRST so we can pass it down to message transformation.
	// Thinking is enabled when the model config explicitly requests it.
	// We honor the config regardless of whether history contains thinking blocks —
	// for models that require reasoning_content round-tripping (DeepSeek), the
	// transformAssistantMessage path injects a placeholder when needed.
	thinkingEnabled := isThinkingEnabled(model)

	// Transform messages with thinking awareness
	messages, err := t.transformMessages(anthropicReq, model.ModelID, thinkingEnabled)
	if err != nil {
		return nil, fmt.Errorf("failed to transform messages: %w", err)
	}

	// Build OpenAI request
	openaiReq := &types.ChatCompletionRequest{
		Model:    model.ModelID,
		Messages: messages,
		Stream:   anthropicReq.Stream,
	}

	// Copy optional parameters from Anthropic request
	if anthropicReq.Temperature != nil {
		openaiReq.Temperature = anthropicReq.Temperature
	}
	if anthropicReq.TopP != nil {
		openaiReq.TopP = anthropicReq.TopP
	}

	// Map max_tokens
	if anthropicReq.MaxTokens > 0 {
		maxTokens := anthropicReq.MaxTokens
		openaiReq.MaxTokens = &maxTokens
	}

	// Apply model-specific overrides
	if model.Temperature > 0 {
		openaiReq.Temperature = &model.Temperature
	}
	if model.MaxTokens > 0 {
		maxTokens := model.MaxTokens
		openaiReq.MaxTokens = &maxTokens
	}

	// Prefer the user's reasoning_effort from the request (Claude Code /effort),
	// then the model config, then default to "high".
	// Provider-specific disable_reasoning_effort is applied post-transform.
	if thinkingEnabled {
		effort := ""
		if anthropicReq.ReasoningEffort != nil && *anthropicReq.ReasoningEffort != "" {
			effort = *anthropicReq.ReasoningEffort
		} else if model.ReasoningEffort != "" {
			effort = model.ReasoningEffort
		}
		if effort != "" {
			openaiReq.ReasoningEffort = &effort
		} else {
			defaultEffort := "high"
			openaiReq.ReasoningEffort = &defaultEffort
		}
		if len(model.Thinking) > 0 {
			openaiReq.Thinking = model.Thinking
		} else {
			openaiReq.Thinking = json.RawMessage(`{"type":"enabled"}`)
		}
	}

	// Transform tools if present
	if len(anthropicReq.Tools) > 0 {
		openaiReq.Tools = t.transformTools(anthropicReq.Tools)
	}

	return openaiReq, nil
}

// isThinkingEnabled returns true when the model config requests thinking mode.
// We treat thinking as enabled when:
//   - Thinking field is present and not explicitly disabled, OR
//   - ReasoningEffort is set (signal that the model should reason)
func isThinkingEnabled(model config.ModelConfig) bool {
	if len(model.Thinking) > 0 {
		// Parse the thinking field to check the type
		var t struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(model.Thinking, &t); err == nil {
			return t.Type != "disabled"
		}
		// Couldn't parse — assume enabled if any thinking config is present
		return true
	}
	return model.ReasoningEffort != ""
}

// HasThinkingBlocks returns true if any assistant message contains a
// thinking content block.
func HasThinkingBlocks(messages []types.Message) bool {
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.ContentBlocks() {
			if block.Type == "thinking" {
				return true
			}
		}
	}
	return false
}

// transformMessages converts Anthropic messages to OpenAI format.
//
// thinkingEnabled signals that the proxy will send thinking mode params upstream;
// when true, assistant messages need to round-trip reasoning_content correctly
// (DeepSeek's validator rejects requests where prior assistant turns lack it).
func (t *RequestTransformer) transformMessages(anthropicReq *types.MessageRequest, modelID string, thinkingEnabled bool) ([]types.ChatMessage, error) {
	var result []types.ChatMessage

	// Add system message if present, preserving cache_control if available
	systemText := anthropicReq.SystemText()
	if systemText != "" {
		systemMsg := types.ChatMessage{
			Role:    "system",
			Content: systemText,
		}
		// Try to extract cache_control from system array blocks
		if len(anthropicReq.System) > 0 {
			var blocks []types.SystemContentBlock
			if err := json.Unmarshal(anthropicReq.System, &blocks); err == nil {
				for _, b := range blocks {
					if b.Type == "text" && b.CacheControl != nil {
						systemMsg.CacheControl = b.CacheControl
						break
					}
				}
			}
		}
		result = append(result, systemMsg)
	}

	// Transform each message
	for _, msg := range anthropicReq.Messages {
		openaiMsgs, err := t.transformMessage(msg, modelID, thinkingEnabled)
		if err != nil {
			return nil, err
		}
		result = append(result, openaiMsgs...)
	}

	return result, nil
}

// transformMessage converts a single Anthropic message to one or more OpenAI messages.
// Tool_use and tool_result require special handling to map to OpenAI's function calling format.
func (t *RequestTransformer) transformMessage(msg types.Message, modelID string, thinkingEnabled bool) ([]types.ChatMessage, error) {
	blocks := msg.ContentBlocks()

	switch msg.Role {
	case "user":
		return t.transformUserMessage(blocks)
	case "assistant":
		return t.transformAssistantMessage(blocks, modelID, thinkingEnabled)
	default:
		// Fallback: concatenate all text
		var text string
		for _, b := range blocks {
			if b.Type == "text" {
				text += b.Text
			}
		}
		return []types.ChatMessage{{Role: msg.Role, Content: text}}, nil
	}
}

// transformUserMessage converts a user message with potential tool_result blocks.
func (t *RequestTransformer) transformUserMessage(blocks []types.ContentBlock) ([]types.ChatMessage, error) {
	var result []types.ChatMessage
	var textParts []string

	for _, block := range blocks {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_result":
			// In OpenAI, tool results are separate messages with role "tool"
			toolContent := block.TextContent()
			result = append(result, types.ChatMessage{
				Role:       "tool",
				Content:    toolContent,
				ToolCallID: block.GetToolID(),
			})
		case "image":
			// Images not supported in text-only models, skip
			textParts = append(textParts, "[Image]")
		}
	}

	// If there's text content, add it as a user message
	if len(textParts) > 0 {
		text := ""
		for _, p := range textParts {
			text += p
		}
		// OpenAI-compatible tool calling requires tool responses to appear
		// immediately after the assistant message that emitted tool_calls.
		// If the Anthropic user turn also includes free-form text, emit it as
		// a subsequent user message after all tool results.
		userMsg := types.ChatMessage{Role: "user", Content: text}
		result = append(result, userMsg)
	}

	return result, nil
}

// transformAssistantMessage converts an assistant message with potential tool_use blocks.
func (t *RequestTransformer) transformAssistantMessage(blocks []types.ContentBlock, modelID string, thinkingEnabled bool) ([]types.ChatMessage, error) {
	var textParts []string
	var thinkingParts []string
	var toolCalls []types.ToolCall

	for _, block := range blocks {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "thinking":
			// Preserve chain-of-thought so it can be forwarded back to providers
			// that require reasoning_content to be preserved across turns.
			if block.Thinking != "" {
				thinkingParts = append(thinkingParts, block.Thinking)
			}
		case "tool_use":
			// Map to OpenAI function call format
			arguments := "{}"
			if len(block.Input) > 0 {
				arguments = string(block.Input)
			}
			toolCalls = append(toolCalls, types.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: types.FunctionCall{
					Name:      block.Name,
					Arguments: arguments,
				},
			})
		}
	}

	// Build the assistant message
	content := ""
	for _, p := range textParts {
		content += p
	}
	reasoningContent := ""
	for _, p := range thinkingParts {
		reasoningContent += p
	}

	var reasoningContentPtr *string
	if reasoningContent != "" {
		// Real thinking content from the upstream history — preserve it.
		reasoningContentPtr = &reasoningContent
	} else if thinkingEnabled && isDeepSeekModel(modelID) {
		// DeepSeek in thinking mode requires reasoning_content on ALL assistant
		// messages — Claude Code strips thinking blocks from history before
		// sending follow-up requests, so we inject a non-empty placeholder.
		// DeepSeek's validator checks for the field's presence, not the content.
		placeholder := " "
		reasoningContentPtr = &placeholder
	} else if len(toolCalls) > 0 && needsPlaceholderReasoning(modelID) {
		// Moonshot's validator treats an empty string as missing, so use a
		// non-empty placeholder when we must provide the field.
		placeholder := " "
		reasoningContentPtr = &placeholder
	}

	msg := types.ChatMessage{
		Role:             "assistant",
		Content:          content,
		ReasoningContent: reasoningContentPtr,
		ToolCalls:        toolCalls,
	}

	return []types.ChatMessage{msg}, nil
}

// transformTools converts Anthropic tools to OpenAI tools.
func (t *RequestTransformer) transformTools(tools []types.Tool) []types.ToolDef {
	var result []types.ToolDef

	for _, tool := range tools {
		// InputSchema is already json.RawMessage, use it directly
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = []byte(`{"type":"object","properties":{}}`)
		}

		result = append(result, types.ToolDef{
			Type: "function",
			Function: types.FunctionDef{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  json.RawMessage(schema),
			},
		})
	}

	return result
}
