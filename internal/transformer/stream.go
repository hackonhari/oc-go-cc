// Package transformer handles request/response transformation and token counting.
package transformer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"oc-go-cc/pkg/types"
)

// ErrClientDisconnected is returned when the client disconnects during streaming.
var ErrClientDisconnected = fmt.Errorf("client disconnected")

// StreamHandler handles streaming SSE transformation from OpenAI to Anthropic format.
type StreamHandler struct {
	responseTransformer *ResponseTransformer
}

// NewStreamHandler creates a new stream handler.
func NewStreamHandler() *StreamHandler {
	return &StreamHandler{
		responseTransformer: NewResponseTransformer(),
	}
}

// ProxyStream takes an OpenAI streaming response and writes Anthropic-format SSE to the writer.
// It reads OpenAI ChatCompletionChunk SSE events and transforms them into Anthropic MessageEvent SSE events.
// The clientCtx is used to detect client disconnection and abort early.
//
// CRITICAL: This function reads directly from resp.Body without buffering to minimize latency.
// Per deep research: "Don't use bufio.Scanner or bufio.Reader on the response body - it adds buffering"
func (h *StreamHandler) ProxyStream(
	w http.ResponseWriter,
	openaiResp io.ReadCloser,
	originalModel string,
	clientCtx context.Context,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported by response writer")
	}

	// Generate a unique message ID for this stream.
	msgID := "msg_" + generateID()

	// Send message_start event with the full message envelope.
	msgStart := types.MessageEvent{
		Type: "message_start",
		Message: &types.MessageResponse{
			ID:      msgID,
			Type:    "message",
			Role:    "assistant",
			Content: []types.ContentBlock{},
			Model:   originalModel,
		},
	}
	if err := writeSSEEvent(w, msgStart); err != nil {
		return ErrClientDisconnected
	}
	flusher.Flush()

	// Read directly from response body without buffering.
	// Use a tight loop with a line buffer - no bufio.Reader.
	contentIndex := 0
	var lineBuf bytes.Buffer
	contentStarted := false
	reasoningStarted := false
	toolUseStarted := false
	// Track the current OpenAI tool_call.index — DeepSeek/OpenAI stream tool args
	// as multiple chunks sharing the same index. -1 means "no tool_use open yet".
	currentToolIndex := -1
	// Some providers omit tool_call.index on later tools; fall back on tool-call id.
	lastToolCallID := ""

	// Read in larger chunks for efficiency, then parse lines
	readBuf := make([]byte, 4096)

	for {
		// Check if client disconnected
		select {
		case <-clientCtx.Done():
			return ErrClientDisconnected
		default:
		}

		// Read chunk from upstream
		n, err := openaiResp.Read(readBuf)
		if n > 0 {
			// Process bytes immediately
			for i := 0; i < n; i++ {
				b := readBuf[i]
				if b == '\n' {
					line := lineBuf.String()
					lineBuf.Reset()

					// Process complete line
					if err := h.processSSELine(w, flusher, line, &contentIndex, &contentStarted, &reasoningStarted, &toolUseStarted, &currentToolIndex, &lastToolCallID, originalModel); err != nil {
						return err
					}
				} else {
					lineBuf.WriteByte(b)
				}
			}
		}

		if err == io.EOF {
			// Process any remaining data in buffer
			if lineBuf.Len() > 0 {
				line := lineBuf.String()
				if err := h.processSSELine(w, flusher, line, &contentIndex, &contentStarted, &reasoningStarted, &toolUseStarted, &currentToolIndex, &lastToolCallID, originalModel); err != nil {
					return err
				}
			}
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read stream: %w", err)
		}
	}

	// Send message_stop event to signal stream completion.
	stopEvent := types.MessageEvent{
		Type: "message_stop",
	}
	if err := writeSSEEvent(w, stopEvent); err != nil {
		return ErrClientDisconnected
	}
	flusher.Flush()

	return nil
}

// processSSELine processes a single SSE line from upstream.
// Per deep research: "Treat SSE primarily as a text protocol" - minimize JSON parsing.
func (h *StreamHandler) processSSELine(
	w http.ResponseWriter,
	flusher http.Flusher,
	line string,
	contentIndex *int,
	contentStarted *bool,
	reasoningStarted *bool,
	toolUseStarted *bool,
	currentToolIndex *int,
	lastToolCallID *string,
	originalModel string,
) error {
	line = strings.TrimSpace(line)

	// Skip empty lines
	if line == "" {
		return nil
	}

	// Skip non-data lines (event: lines, id: lines, etc.)
	if !strings.HasPrefix(line, "data: ") {
		return nil
	}

	data := strings.TrimPrefix(line, "data: ")
	if data == "" {
		return nil
	}
	// Process the SSE data line

	// Handle [DONE] marker
	if data == "[DONE]" {
		return nil
	}

	// Fast path: check if this is a content chunk without full JSON parsing.
	// Skip the fast path when reasoning_content is also present in the same
	// chunk — falling through to JSON parsing ensures both fields are handled
	// correctly. Otherwise reasoning_content gets silently dropped, and on the
	// next turn DeepSeek rejects the request with:
	//   "The reasoning_content in the thinking mode must be passed back to the API."
	if !strings.Contains(data, `"reasoning_content"`) && !strings.Contains(data, `"reasoning":`) {
		if idx := strings.Index(data, `"delta":{"content":"`); idx != -1 {
			// Extract content directly
			start := idx + len(`"delta":{"content":"`)
			// Scan for closing quote, skipping JSON-escaped chars.
			// A naive Index search breaks on \" inside the content
			// (e.g. "hello" → extracted as just "\").
			end := -1
			for i := 0; i < len(data[start:]); i++ {
				if data[start+i] == '\\' {
					i++ // skip JSON-escaped character
				} else if data[start+i] == '"' {
					end = i
					break
				}
			}
			if end != -1 {
				content := data[start : start+end]
				if content != "" {
					// Unescape JSON escape sequences that the fast path's raw string
					// extraction picks up literally (e.g. \n becomes actual newline).
					content = unescapeJSON(content)
					if !*contentStarted {
						// If reasoning was already started, close it (with signature_delta) first
						if *reasoningStarted {
							if err := closeContentBlock(w, contentIndex, true); err != nil {
								return ErrClientDisconnected
							}
							*contentIndex++
							*reasoningStarted = false
						}
						*contentStarted = true
						// Send content_block_start with proper Anthropic spec format:
						// uses content_block field, not delta
						startEvent := types.MessageEvent{
							Type:         "content_block_start",
							Index:        contentIndex,
							ContentBlock: &types.ContentBlock{Type: "text", Text: ""},
						}
						if err := writeSSEEvent(w, startEvent); err != nil {
							return ErrClientDisconnected
						}
					}

					// Send content_block_delta
					delta := types.Delta{
						Type: "text_delta",
						Text: content,
					}
					event := types.MessageEvent{
						Type:  "content_block_delta",
						Index: contentIndex,
						Delta: &delta,
					}
					if err := writeSSEEvent(w, event); err != nil {
						return ErrClientDisconnected
					}
					flusher.Flush()
				}
				return nil
			}
		}
	}

	// Check for finish_reason — close blocks and emit message_delta.
	// IMPORTANT: do not take this string fast-path when this same SSE line also
	// carries tool_calls or reasoning_content. Those must be processed via JSON
	// unmarshaling below; otherwise the last chunk can drop final tool deltas
	// (breaking the 2nd+ parallel tool) or lose reasoning tokens.
	if strings.Contains(data, `"finish_reason":`) && !strings.Contains(data, `"finish_reason":null`) &&
		!strings.Contains(data, `"tool_calls"`) && !strings.Contains(data, `"reasoning_content"`) && !strings.Contains(data, `"reasoning":`) {
		// Close any open content block (reasoning, text, or tool_use).
		// Thinking blocks must emit signature_delta before content_block_stop.
		if *contentStarted || *reasoningStarted || *toolUseStarted {
			if err := closeContentBlock(w, contentIndex, *reasoningStarted); err != nil {
				return ErrClientDisconnected
			}
			*contentStarted = false
			*reasoningStarted = false
			*toolUseStarted = false
		}

		// Map OpenAI finish_reason to Anthropic stop_reason. The fast-path needs
		// this for chunks that contain only finish_reason metadata (no full JSON
		// parse). tool_calls → tool_use is the most important mapping; without
		// it, Claude Code may not realize the assistant invoked a tool.
		stopReason := "end_turn"
		if strings.Contains(data, `"finish_reason":"tool_calls"`) {
			stopReason = "tool_use"
		} else if strings.Contains(data, `"finish_reason":"length"`) {
			stopReason = "max_tokens"
		} else if strings.Contains(data, `"finish_reason":"stop"`) {
			stopReason = "end_turn"
		}

		msgDelta := types.MessageEvent{
			Type: "message_delta",
			Delta: &types.Delta{
				StopReason: stopReason,
			},
		}
		if err := writeSSEEvent(w, msgDelta); err != nil {
			return ErrClientDisconnected
		}
		flusher.Flush()
		return nil
	}

	// For tool calls and other complex cases, fall back to full JSON parsing
	var chunk types.ChatCompletionChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		// Skip malformed chunks - don't fail the whole stream
		return nil
	}

	if len(chunk.Choices) == 0 {
		return nil
	}

	choice := chunk.Choices[0]

	// Handle reasoning content deltas — both standard reasoning_content
	// and Command Code's non-standard "reasoning" field.
	reasoningContent := ""
	if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
		reasoningContent = *choice.Delta.ReasoningContent
	} else if choice.Delta.Reasoning != nil && *choice.Delta.Reasoning != "" {
		reasoningContent = *choice.Delta.Reasoning
	}
	if reasoningContent != "" {
		if !*reasoningStarted {
			// If text was already started, close it first (no signature for text)
			if *contentStarted {
				if err := closeContentBlock(w, contentIndex, false); err != nil {
					return ErrClientDisconnected
				}
				*contentIndex++
				*contentStarted = false
			}
			*reasoningStarted = true
			// Per Anthropic spec, content_block_start uses content_block field
			startEvent := types.MessageEvent{
				Type:         "content_block_start",
				Index:        contentIndex,
				ContentBlock: &types.ContentBlock{Type: "thinking", Thinking: ""},
			}
			if err := writeSSEEvent(w, startEvent); err != nil {
				return ErrClientDisconnected
			}
		}

		delta := types.Delta{
			Type:     "thinking_delta",
			Thinking: reasoningContent,
		}
		event := types.MessageEvent{
			Type:  "content_block_delta",
			Index: contentIndex,
			Delta: &delta,
		}
		if err := writeSSEEvent(w, event); err != nil {
			return ErrClientDisconnected
		}
		flusher.Flush()
	}

	// Handle text content deltas
	if choice.Delta.Content != "" {
		if !*contentStarted {
			// If reasoning was already started, close it (with signature_delta) first
			if *reasoningStarted {
				if err := closeContentBlock(w, contentIndex, true); err != nil {
					return ErrClientDisconnected
				}
				*contentIndex++
				*reasoningStarted = false
			}
			*contentStarted = true
			// Per Anthropic spec, content_block_start uses content_block field
			startEvent := types.MessageEvent{
				Type:         "content_block_start",
				Index:        contentIndex,
				ContentBlock: &types.ContentBlock{Type: "text", Text: ""},
			}
			if err := writeSSEEvent(w, startEvent); err != nil {
				return ErrClientDisconnected
			}
		}

		delta := types.Delta{
			Type: "text_delta",
			Text: choice.Delta.Content,
		}
		event := types.MessageEvent{
			Type:  "content_block_delta",
			Index: contentIndex,
			Delta: &delta,
		}
		if err := writeSSEEvent(w, event); err != nil {
			return ErrClientDisconnected
		}
		flusher.Flush()
	}

	// Handle tool call deltas.
	// OpenAI/DeepSeek streams tool_call argument fragments across many chunks,
	// each sharing the same tool_call.index. We must dedupe — only open a new
	// content_block when tc.Index differs from currentToolIndex; otherwise emit
	// input_json_delta to extend the open tool_use block.
	if len(choice.Delta.ToolCalls) > 0 {
		for _, tc := range choice.Delta.ToolCalls {
			tcIdx := 0
			hasIdx := false
			if tc.Index != nil {
				tcIdx = *tc.Index
				hasIdx = true
			}

			// New tool_use when OpenAI index advances, or when index is missing
			// but a new non-empty tool id appears (some providers omit index on 2nd tool).
			isNewToolCall := false
			if hasIdx {
				isNewToolCall = *currentToolIndex != tcIdx
			} else if tc.ID != "" && tc.ID != *lastToolCallID {
				isNewToolCall = true
			}

			if isNewToolCall {
				// Close any prior open block (text/thinking/tool_use) per Anthropic spec.
				// Thinking blocks must emit signature_delta before content_block_stop.
				if *contentStarted || *reasoningStarted || *toolUseStarted {
					if err := closeContentBlock(w, contentIndex, *reasoningStarted); err != nil {
						return ErrClientDisconnected
					}
					*contentStarted = false
					*reasoningStarted = false
					*toolUseStarted = false
				}

				*contentIndex++
				*toolUseStarted = true
				if hasIdx {
					*currentToolIndex = tcIdx
				} else {
					*currentToolIndex++
				}
				if tc.ID != "" {
					*lastToolCallID = tc.ID
				}

				// content_block_start carries id+name+input per Anthropic spec.
				startEvent := types.MessageEvent{
					Type:  "content_block_start",
					Index: contentIndex,
					ContentBlock: &types.ContentBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: json.RawMessage(`{}`),
					},
				}
				if err := writeSSEEvent(w, startEvent); err != nil {
					return ErrClientDisconnected
				}
			}

			// Stream the partial JSON for arguments — every chunk that has args
			// (whether opening or continuing) emits an input_json_delta.
			if tc.Function.Arguments != "" {
				delta := types.Delta{
					Type:        "input_json_delta",
					PartialJSON: tc.Function.Arguments,
				}
				event := types.MessageEvent{
					Type:  "content_block_delta",
					Index: contentIndex,
					Delta: &delta,
				}
				if err := writeSSEEvent(w, event); err != nil {
					return ErrClientDisconnected
				}
			}
			flusher.Flush()
		}
	}

	// Handle finish reason
	if choice.FinishReason != "" {
		// Close any open content block (reasoning, text, or tool_use).
		// Thinking blocks must emit signature_delta before content_block_stop.
		if *contentStarted || *reasoningStarted || *toolUseStarted {
			if err := closeContentBlock(w, contentIndex, *reasoningStarted); err != nil {
				return ErrClientDisconnected
			}
			*contentStarted = false
			*reasoningStarted = false
			*toolUseStarted = false
		}

		var usage *types.Usage
		if chunk.Usage != nil {
			usage = &types.Usage{
				InputTokens:              chunk.Usage.PromptTokens,
				OutputTokens:             chunk.Usage.CompletionTokens,
				CacheCreationInputTokens: chunk.Usage.PromptCacheMissTokens,
				CacheReadInputTokens:     chunk.Usage.PromptCacheHitTokens,
			}
		}

		msgDelta := types.MessageEvent{
			Type: "message_delta",
			Delta: &types.Delta{
				StopReason: h.responseTransformer.mapFinishReason(choice.FinishReason),
			},
			Usage: usage,
		}
		if err := writeSSEEvent(w, msgDelta); err != nil {
			return ErrClientDisconnected
		}
		flusher.Flush()
	}

	return nil
}

// closeContentBlock emits the correct closing sequence for a content block.
// For thinking blocks, Anthropic spec requires a signature_delta event before
// content_block_stop — without it, Claude Code's parser fails. Since DeepSeek
// doesn't provide a real signature, we emit a placeholder; Claude Code only
// needs the field present to advance its parser state.
func closeContentBlock(w http.ResponseWriter, index *int, isThinking bool) error {
	if isThinking {
		sigDelta := types.MessageEvent{
			Type:  "content_block_delta",
			Index: index,
			Delta: &types.Delta{
				Type:      "signature_delta",
				Signature: "proxy_synthesized",
			},
		}
		if err := writeSSEEvent(w, sigDelta); err != nil {
			return err
		}
	}
	stopEvent := types.MessageEvent{
		Type:  "content_block_stop",
		Index: index,
	}
	return writeSSEEvent(w, stopEvent)
}

// writeSSEEvent writes a single SSE event to the HTTP response writer.
// Format: "event: <type>\ndata: <json>\n\n"
func writeSSEEvent(w http.ResponseWriter, event types.MessageEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(data))
	return err
}

// unescapeJSON replaces JSON escape sequences in a fast-path extracted string.
// The fast path extracts content from raw JSON without unmarshaling, so escape
// sequences like \n, \t, \\, \" appear as literal characters. This function
// unescapes only the sequences that commonly appear in text content.
func unescapeJSON(s string) string {
	s = strings.ReplaceAll(s, "\\n", "\n")
	s = strings.ReplaceAll(s, "\\t", "\t")
	s = strings.ReplaceAll(s, "\\r", "\r")
	s = strings.ReplaceAll(s, "\\\"", "\"")
	s = strings.ReplaceAll(s, "\\\\", "\\")
	return s
}

// generateID creates a unique identifier based on current time.
func generateID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

