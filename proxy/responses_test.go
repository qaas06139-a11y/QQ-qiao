package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesToClaudeRequestStringInput(t *testing.T) {
	req := &ResponsesRequest{
		Model: "claude-sonnet-4.5",
		Input: "hello",
	}
	c := responsesToClaudeRequest(req)
	if len(c.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(c.Messages))
	}
	if c.Messages[0].Role != "user" || c.Messages[0].Content != "hello" {
		t.Fatalf("unexpected message: %+v", c.Messages[0])
	}
}

func TestResponsesToClaudeRequestArrayInput(t *testing.T) {
	raw := `{
		"model": "claude-sonnet-4.5",
		"instructions": "be concise",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "find weather"}]},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"Tokyo\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "sunny, 22C"},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "thanks"}]}
		]
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := responsesToClaudeRequest(&req)

	if c.System != "be concise" {
		t.Fatalf("system not preserved: %v", c.System)
	}
	if len(c.Messages) != 3 {
		t.Fatalf("expected 3 messages (user, assistant tool_use, user with tool_result+text), got %d", len(c.Messages))
	}
	if c.Messages[0].Role != "user" {
		t.Fatalf("first message role: %s", c.Messages[0].Role)
	}
	if c.Messages[1].Role != "assistant" {
		t.Fatalf("second message role: %s", c.Messages[1].Role)
	}
	if c.Messages[2].Role != "user" {
		t.Fatalf("third message role: %s", c.Messages[2].Role)
	}
	// Validate tool_result block was merged onto the trailing user turn.
	blocks, ok := c.Messages[2].Content.([]interface{})
	if !ok {
		t.Fatalf("expected last user turn to be structured blocks, got %T", c.Messages[2].Content)
	}
	hasToolResult := false
	for _, b := range blocks {
		if m, ok := b.(map[string]interface{}); ok && m["type"] == "tool_result" {
			hasToolResult = true
			if m["tool_use_id"] != "call_1" {
				t.Fatalf("tool_result tool_use_id mismatch: %v", m["tool_use_id"])
			}
		}
	}
	if !hasToolResult {
		t.Fatalf("tool_result block not merged onto user turn")
	}
}

func TestConvertResponsesTools(t *testing.T) {
	tools := []ResponsesTool{
		{Type: "function", Name: "get_weather", Description: "Get weather", Parameters: map[string]interface{}{"type": "object"}},
		{Type: "computer_use", Name: "should_be_dropped"},
		{Name: "no_type_ok"},
	}
	out := convertResponsesTools(tools)
	if len(out) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(out))
	}
	names := []string{out[0].Name, out[1].Name}
	if !contains(names, "get_weather") || !contains(names, "no_type_ok") {
		t.Fatalf("unexpected tools: %+v", names)
	}
}

func TestReasoningEffortRequestsThinking(t *testing.T) {
	cases := map[string]bool{
		"":         false,
		"none":     false,
		"minimal":  false,
		"off":      false,
		"low":      true,
		"medium":   true,
		"high":     true,
		"  HIGH  ": true,
	}
	for effort, want := range cases {
		got := reasoningEffortRequestsThinking(&ResponsesReasoning{Effort: effort})
		if got != want {
			t.Fatalf("reasoningEffortRequestsThinking(%q) = %v, want %v", effort, got, want)
		}
	}
	if reasoningEffortRequestsThinking(nil) {
		t.Fatalf("nil reasoning should not request thinking")
	}
}

func TestBuildResponsesResponseShape(t *testing.T) {
	resp := buildResponsesResponse("claude-sonnet-4.5", "Hello", "thinking out loud",
		[]KiroToolUse{{ToolUseID: "call_1", Name: "get_weather", Input: map[string]interface{}{"city": "Tokyo"}}},
		100, 50)

	if resp.Object != "response" {
		t.Fatalf("expected object=response, got %s", resp.Object)
	}
	if resp.Usage.TotalTokens != 150 {
		t.Fatalf("total tokens = %d, want 150", resp.Usage.TotalTokens)
	}
	if resp.OutputText != "Hello" {
		t.Fatalf("output_text shorthand mismatch: %q", resp.OutputText)
	}
	if len(resp.Output) != 3 {
		t.Fatalf("expected 3 output items (reasoning + message + function_call), got %d", len(resp.Output))
	}
	if resp.Output[0].Type != "reasoning" {
		t.Fatalf("first output should be reasoning, got %s", resp.Output[0].Type)
	}
	if resp.Output[1].Type != "message" {
		t.Fatalf("second output should be message, got %s", resp.Output[1].Type)
	}
	if resp.Output[2].Type != "function_call" {
		t.Fatalf("third output should be function_call, got %s", resp.Output[2].Type)
	}
	if !strings.Contains(resp.Output[2].Arguments, "Tokyo") {
		t.Fatalf("function_call arguments missing payload: %s", resp.Output[2].Arguments)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestResponsesToClaudeRequestImageInput(t *testing.T) {
	raw := `{
		"model": "claude-sonnet-4.5",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [
					{"type": "input_text", "text": "What's in this image?"},
					{"type": "input_image", "image_url": "data:image/png;base64,iVBORw0KGgo=", "mime_type": "image/png"}
				]
			}
		]
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := responsesToClaudeRequest(&req)
	if len(c.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(c.Messages))
	}
	blocks, ok := c.Messages[0].Content.([]interface{})
	if !ok {
		t.Fatalf("expected structured blocks, got %T", c.Messages[0].Content)
	}
	hasText, hasImage := false, false
	for _, b := range blocks {
		m, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			hasText = true
		case "image":
			hasImage = true
		}
	}
	if !hasText || !hasImage {
		t.Fatalf("expected both text and image blocks, got text=%v image=%v", hasText, hasImage)
	}
}

func TestResponsesToClaudeRequestDropsReasoningEcho(t *testing.T) {
	raw := `{
		"model": "claude-sonnet-4.5",
		"input": [
			{"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "previous chain of thought"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "follow up"}]}
		]
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := responsesToClaudeRequest(&req)
	if len(c.Messages) != 1 {
		t.Fatalf("expected 1 message after dropping reasoning echo, got %d", len(c.Messages))
	}
	if c.Messages[0].Role != "user" || c.Messages[0].Content != "follow up" {
		t.Fatalf("unexpected message: %+v", c.Messages[0])
	}
}

func TestResponsesToClaudeRequestStringFunctionOutput(t *testing.T) {
	// function_call_output should always become a tool_result block on the
	// trailing user turn; here it stands alone with no following user
	// message, so we expect a synthesised user turn.
	raw := `{
		"model": "claude-sonnet-4.5",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "run ls"}]},
			{"type": "function_call", "call_id": "call_1", "name": "run", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "a.txt\nb.txt"}
		]
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := responsesToClaudeRequest(&req)
	if len(c.Messages) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, user-tool_result), got %d", len(c.Messages))
	}
	last := c.Messages[2]
	if last.Role != "user" {
		t.Fatalf("trailing tool_result should be wrapped in user message, got role %s", last.Role)
	}
	blocks, ok := last.Content.([]interface{})
	if !ok || len(blocks) != 1 {
		t.Fatalf("expected single tool_result block, got %T %v", last.Content, last.Content)
	}
	m := blocks[0].(map[string]interface{})
	if m["type"] != "tool_result" {
		t.Fatalf("expected tool_result, got %v", m["type"])
	}
}

