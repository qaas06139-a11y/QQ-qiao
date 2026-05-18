package proxy

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// =====================================================================
// OpenAI Responses API support.
//
// The Codex CLI (and the newer ChatGPT/Agents stack) uses the Responses API
// instead of Chat Completions. The wire format differs in three big ways:
//
//   1. Top-level shape: requests carry "input" (array of typed items) plus
//      "instructions" instead of "messages" + "system".
//   2. Each output is wrapped in an "item" with a stable id and a content
//      array using "output_text" / "reasoning" parts.
//   3. Streaming uses fine-grained event names like
//      response.output_text.delta / response.function_call_arguments.delta
//      rather than the OpenAI Chat "data: {chunk}" / "data: [DONE]" stream.
//
// Translation strategy: convert Responses requests into the same internal
// ClaudeRequest used elsewhere, reuse ClaudeToKiro to build the Kiro payload,
// and emit Responses-shaped streaming events from the Kiro callback.
// =====================================================================

// ResponsesRequest is the subset of the OpenAI Responses API that we accept.
type ResponsesRequest struct {
	Model           string             `json:"model"`
	Input           interface{}        `json:"input"` // string | []ResponsesInputItem
	Instructions    string             `json:"instructions,omitempty"`
	MaxOutputTokens int                `json:"max_output_tokens,omitempty"`
	Temperature     float64            `json:"temperature,omitempty"`
	TopP            float64            `json:"top_p,omitempty"`
	Stream          bool               `json:"stream,omitempty"`
	Reasoning       *ResponsesReasoning `json:"reasoning,omitempty"`
	Tools           []ResponsesTool    `json:"tools,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	// PreviousResponseID continues a server-stored session; the proxy
	// looks up the saved history under this id and prepends it to the
	// current Input so the client doesn't have to resend the conversation.
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	// Store, when explicitly false, opts the request out of session
	// persistence on the server. Defaults to true (matches OpenAI).
	Store *bool `json:"store,omitempty"`

	// Tolerated but ignored: metadata, user, etc.
}

// ResponsesReasoning configures reasoning depth. Codex currently sets
// effort=low|medium|high; we map any non-empty value to thinking mode.
type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ResponsesTool mirrors the simplified flat tool definition used by Responses
// (no nested "function" object as in Chat Completions).
type ResponsesTool struct {
	Type        string      `json:"type"`
	Name        string      `json:"name,omitempty"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
	Strict      *bool       `json:"strict,omitempty"`
}

// ResponsesUsage matches the Responses-style token usage block we emit.
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponsesOutputContentPart is one entry inside an item's content array.
type ResponsesOutputContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ResponsesOutputItem represents a top-level output item: assistant message,
// function_call, or reasoning.
type ResponsesOutputItem struct {
	Type      string                       `json:"type"`
	ID        string                       `json:"id"`
	Status    string                       `json:"status,omitempty"`
	Role      string                       `json:"role,omitempty"`
	Content   []ResponsesOutputContentPart `json:"content,omitempty"`
	CallID    string                       `json:"call_id,omitempty"`
	Name      string                       `json:"name,omitempty"`
	Arguments string                       `json:"arguments,omitempty"`
	Summary   []ResponsesOutputContentPart `json:"summary,omitempty"`
}

// ResponsesResponse is the non-stream response body.
type ResponsesResponse struct {
	ID         string                `json:"id"`
	Object     string                `json:"object"`
	CreatedAt  int64                 `json:"created_at"`
	Status     string                `json:"status"`
	Model      string                `json:"model"`
	Output     []ResponsesOutputItem `json:"output"`
	Usage      ResponsesUsage        `json:"usage"`
	OutputText string                `json:"output_text,omitempty"`
}

// ====================== Request -> internal Claude shape ======================

// responsesToClaudeRequest folds a Responses request into the internal
// ClaudeRequest used everywhere else in this proxy. Tool history (previous
// function_call / function_call_output items) becomes assistant tool_use +
// user tool_result messages, mirroring how Anthropic represents the same
// conversation.
func responsesToClaudeRequest(req *ResponsesRequest) *ClaudeRequest {
	out := &ClaudeRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}
	if req.Instructions != "" {
		out.System = req.Instructions
	}

	out.Messages = buildClaudeMessagesFromResponsesInput(req.Input)
	out.Tools = convertResponsesTools(req.Tools)
	return out
}

// prependSessionHistory inserts the messages reconstructed from a stored
// previous_response_id chain in front of the request's own messages. The
// caller is responsible for filtering out turns that are already echoed
// back via Responses input items (Codex commonly resends recent history).
func prependSessionHistory(req *ClaudeRequest, history []SessionTurn) {
	if req == nil || len(history) == 0 {
		return
	}
	prefix := make([]ClaudeMessage, 0, len(history)*2)
	for _, turn := range history {
		if turn.UserContent != nil {
			prefix = append(prefix, ClaudeMessage{Role: "user", Content: turn.UserContent})
		}
		assistantBlocks := assistantBlocksFromTurn(turn)
		if assistantBlocks != nil {
			prefix = append(prefix, ClaudeMessage{Role: "assistant", Content: assistantBlocks})
		}
		if len(turn.UserToolResults) > 0 {
			prefix = append(prefix, ClaudeMessage{Role: "user", Content: turn.UserToolResults})
		}
	}
	if len(prefix) == 0 {
		return
	}
	req.Messages = append(prefix, req.Messages...)
}

func assistantBlocksFromTurn(turn SessionTurn) interface{} {
	if turn.AssistantText == "" && len(turn.ToolUses) == 0 {
		return nil
	}
	blocks := make([]interface{}, 0, 1+len(turn.ToolUses))
	if turn.AssistantText != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "text",
			"text": turn.AssistantText,
		})
	}
	for _, tu := range turn.ToolUses {
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    tu.ToolUseID,
			"name":  tu.Name,
			"input": tu.Input,
		})
	}
	if len(blocks) == 1 {
		// Keep simple text turns as plain strings to match the rest of
		// the codebase's expectations.
		if m, ok := blocks[0].(map[string]interface{}); ok && m["type"] == "text" {
			if t, ok := m["text"].(string); ok {
				return t
			}
		}
	}
	return blocks
}

// extractCurrentTurnUserContent inspects a Responses request's input and
// returns the user-side content of the *last* user-role message. Used when
// persisting the turn to the session store so a follow-up request with
// previous_response_id can replay it.
func extractCurrentTurnUserContent(input interface{}) interface{} {
	msgs := buildClaudeMessagesFromResponsesInput(input)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return nil
}

// buildClaudeMessagesFromResponsesInput walks the Responses "input" payload
// and returns the equivalent ordered list of Claude-format messages.
func buildClaudeMessagesFromResponsesInput(input interface{}) []ClaudeMessage {
	if input == nil {
		return nil
	}

	// String shorthand: a single user message.
	if s, ok := input.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		return []ClaudeMessage{{Role: "user", Content: s}}
	}

	items, ok := input.([]interface{})
	if !ok {
		return nil
	}

	var messages []ClaudeMessage
	// Pending tool_result blocks waiting to be flushed onto the next user
	// message (Claude requires tool_result blocks inside a user turn).
	var pendingToolResults []map[string]interface{}

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		blocks := make([]interface{}, 0, len(pendingToolResults))
		for _, b := range pendingToolResults {
			blocks = append(blocks, b)
		}
		messages = append(messages, ClaudeMessage{Role: "user", Content: blocks})
		pendingToolResults = nil
	}

	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		itemType, _ := item["type"].(string)
		switch itemType {
		case "", "message":
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			content := convertResponsesMessageContent(item["content"], role)
			if role == "user" {
				// Merge any pending tool results with this user turn.
				if len(pendingToolResults) > 0 {
					blocks := make([]interface{}, 0, len(pendingToolResults)+1)
					for _, b := range pendingToolResults {
						blocks = append(blocks, b)
					}
					switch c := content.(type) {
					case string:
						if strings.TrimSpace(c) != "" {
							blocks = append(blocks, map[string]interface{}{"type": "text", "text": c})
						}
					case []interface{}:
						blocks = append(blocks, c...)
					}
					content = blocks
					pendingToolResults = nil
				}
			} else {
				flushToolResults()
			}
			messages = append(messages, ClaudeMessage{Role: role, Content: content})

		case "function_call":
			// Previous-turn assistant tool call.
			flushToolResults()
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name, _ := item["name"].(string)
			argsStr, _ := item["arguments"].(string)
			var input map[string]interface{}
			if argsStr != "" {
				_ = json.Unmarshal([]byte(argsStr), &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			messages = append(messages, ClaudeMessage{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type":  "tool_use",
						"id":    callID,
						"name":  name,
						"input": input,
					},
				},
			})

		case "function_call_output":
			callID, _ := item["call_id"].(string)
			output, _ := item["output"].(string)
			if output == "" {
				if raw, ok := item["output"]; ok {
					if b, err := json.Marshal(raw); err == nil {
						output = string(b)
					}
				}
			}
			pendingToolResults = append(pendingToolResults, map[string]interface{}{
				"type":         "tool_result",
				"tool_use_id":  callID,
				"content":      output,
			})

		case "reasoning":
			// Echoed reasoning items from a previous turn are dropped: we
			// don't replay private chain-of-thought back to the model.
			continue

		default:
			// Unknown items become best-effort user text so we don't lose
			// context, but we never error out.
			if b, err := json.Marshal(item); err == nil {
				messages = append(messages, ClaudeMessage{Role: "user", Content: string(b)})
			}
		}
	}

	flushToolResults()
	return messages
}

// convertResponsesMessageContent normalises a Responses content array
// (input_text / output_text / input_image parts) into the polymorphic shape
// our existing Claude extractor accepts.
func convertResponsesMessageContent(content interface{}, role string) interface{} {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]interface{})
	if !ok {
		return ""
	}

	blocks := make([]interface{}, 0, len(parts))
	for _, raw := range parts {
		p, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		pType, _ := p["type"].(string)
		switch pType {
		case "input_text", "output_text", "text":
			text, _ := p["text"].(string)
			if text == "" {
				continue
			}
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": text})
		case "input_image":
			// Pass through as a Claude-style image block; the existing
			// extractor handles data URLs and bare base64.
			source := map[string]interface{}{"type": "base64"}
			if url, ok := p["image_url"].(string); ok && url != "" {
				source["data"] = url
			}
			if mt, ok := p["mime_type"].(string); ok {
				source["media_type"] = mt
			}
			blocks = append(blocks, map[string]interface{}{
				"type":   "image",
				"source": source,
			})
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 {
		// Optimisation: keep simple text turns as plain strings to match the
		// shape produced by the rest of the codebase.
		if m, ok := blocks[0].(map[string]interface{}); ok && m["type"] == "text" {
			if t, ok := m["text"].(string); ok {
				return t
			}
		}
	}
	return blocks
}

// convertResponsesTools maps Responses-style flat tools into the internal
// Claude tool shape (which is what ClaudeToKiro expects).
func convertResponsesTools(tools []ResponsesTool) []ClaudeTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ClaudeTool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Name == "" {
			continue
		}
		out = append(out, ClaudeTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	return out
}

// reasoningEffortRequestsThinking returns true when the request asks for any
// non-trivial reasoning effort. Kiro-Go has a single "thinking" toggle so we
// collapse all non-empty efforts onto it.
func reasoningEffortRequestsThinking(r *ResponsesReasoning) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Effort)) {
	case "", "none", "minimal", "off":
		return false
	}
	return true
}

// newResponseID builds a stable id for a new Responses response.
func newResponseID() string {
	return "resp_" + uuid.New().String()
}

// newOutputItemID builds an id for an individual output item.
func newOutputItemID(prefix string) string {
	if prefix == "" {
		prefix = "msg"
	}
	return prefix + "_" + uuid.New().String()
}
