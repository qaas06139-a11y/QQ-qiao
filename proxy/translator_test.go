package proxy

import (
	"strings"
	"testing"
)

func TestExtractOpenAIMessageTextStructured(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "alpha"},
		map[string]interface{}{"type": "input_text", "text": "beta"},
	}

	if got := extractOpenAIMessageText(content); got != "alphabeta" {
		t.Fatalf("expected concatenated structured text, got %q", got)
	}

	nested := map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "nested"}},
	}
	if got := extractOpenAIMessageText(nested); got != "nested" {
		t.Fatalf("expected nested content extraction, got %q", got)
	}
}

func TestOpenAIToKiroPreservesStructuredAssistantAndToolContent(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{
				Role: "system",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "system-a"},
					map[string]interface{}{"type": "text", "text": "system-b"},
				},
			},
			{Role: "user", Content: "first-question"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "assistant-structured"},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "tool-result-structured"},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)

	if len(payload.ConversationState.History) != 2 {
		t.Fatalf("expected 2 history items, got %d", len(payload.ConversationState.History))
	}

	firstHistoryUser := payload.ConversationState.History[0].UserInputMessage
	if firstHistoryUser == nil {
		t.Fatalf("expected first history item to be user message")
	}
	if !strings.Contains(firstHistoryUser.Content, "system-a") ||
		!strings.Contains(firstHistoryUser.Content, "system-b") ||
		!strings.Contains(firstHistoryUser.Content, "first-question") {
		t.Fatalf("expected merged system+user content, got %q", firstHistoryUser.Content)
	}

	historyAssistant := payload.ConversationState.History[1].AssistantResponseMessage
	if historyAssistant == nil {
		t.Fatalf("expected second history item to be assistant message")
	}
	if historyAssistant.Content != "assistant-structured" {
		t.Fatalf("expected assistant structured content to be preserved, got %q", historyAssistant.Content)
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "tool-result-structured") {
		t.Fatalf("expected tool-result continuation content, got %q", cur.Content)
	}
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result in current context")
	}
	gotToolText := cur.UserInputMessageContext.ToolResults[0].Content[0].Text
	if gotToolText != "tool-result-structured" {
		t.Fatalf("expected structured tool result text, got %q", gotToolText)
	}
}

func TestOpenAIToKiroAssistantMapContentInHistory(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: map[string]interface{}{"type": "text", "text": "assistant-map"}},
			{Role: "user", Content: "u2"},
		},
	}

	payload := OpenAIToKiro(req, false)

	if len(payload.ConversationState.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(payload.ConversationState.History))
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil {
		t.Fatalf("expected second history entry to be assistant")
	}
	if assistant.Content != "assistant-map" {
		t.Fatalf("expected assistant map content preserved, got %q", assistant.Content)
	}
}

func TestOpenAIToKiroAssistantToolCallsDoNotInjectPlaceholder(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role:    "assistant",
				Content: nil,
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "user", Content: "continue"},
		},
	}

	payload := OpenAIToKiro(req, false)
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected history with assistant tool call")
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil {
		t.Fatalf("expected assistant history entry")
	}
	if assistant.Content != "" {
		t.Fatalf("expected empty assistant content for tool-call-only turn, got %q", assistant.Content)
	}
}

func TestOpenAIConversationIDStableFromAnchor(t *testing.T) {
	baseMessages := []OpenAIMessage{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "Build calculator"},
		{Role: "assistant", Content: "Sure"},
		{Role: "user", Content: "Continue"},
	}

	reqA := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: baseMessages}
	reqB := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: append(baseMessages, OpenAIMessage{Role: "assistant", Content: "Next step"})}

	payloadA := OpenAIToKiro(reqA, false)
	payloadB := OpenAIToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestClaudeConversationIDStableFromAnchor(t *testing.T) {
	reqA := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	reqB := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "next"},
		},
	}

	payloadA := ClaudeToKiro(reqA, false)
	payloadB := ClaudeToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestOpenAIConversationIDRandomForSyntheticAnchor(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "assistant", Content: "prefill"},
		},
	}

	payloadA := OpenAIToKiro(req, false)
	payloadB := OpenAIToKiro(req, false)

	if payloadA.ConversationState.ConversationID == payloadB.ConversationState.ConversationID {
		t.Fatalf("expected synthetic anchor to generate non-deterministic conversation IDs")
	}
}

func TestClaudeToKiroDropsLeadingAssistantHistory(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: "prefill"},
			{Role: "user", Content: "real user message"},
		},
	}

	payload := ClaudeToKiro(req, false)

	if len(payload.ConversationState.History) != 0 {
		t.Fatalf("expected leading assistant-only history to be dropped, got %d entries", len(payload.ConversationState.History))
	}

	if strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "Begin conversation") {
		t.Fatalf("unexpected synthetic Begin conversation injection in current content: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

func TestKiroToClaudeResponseCanEmitEmptyThinkingBlock(t *testing.T) {
	resp := KiroToClaudeResponse("final answer", "", true, nil, 10, 20, "claude-sonnet-4.6")

	if len(resp.Content) != 2 {
		t.Fatalf("expected empty thinking block plus text block, got %d blocks", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" {
		t.Fatalf("expected first block to be thinking, got %#v", resp.Content[0])
	}
	if resp.Content[0].Thinking != "" {
		t.Fatalf("expected omitted thinking block to have empty content, got %#v", resp.Content[0].Thinking)
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "final answer" {
		t.Fatalf("expected text block to be preserved, got %#v", resp.Content[1])
	}
}

func TestToolResultsContinuationIncludesInstructionPrefix(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find data"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "fetch", Arguments: "{}"},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "result-1"},
		},
	}

	payload := OpenAIToKiro(req, false)
	content := payload.ConversationState.CurrentMessage.UserInputMessage.Content

	if !strings.Contains(content, toolResultsContinuationPrefix) {
		t.Fatalf("expected tool continuation prefix, got %q", content)
	}
	if !strings.Contains(content, "result-1") {
		t.Fatalf("expected tool result text in continuation content, got %q", content)
	}
}

func TestParseModelAndThinkingFallback(t *testing.T) {
	cases := []struct {
		input        string
		wantModel    string
		wantThinking bool
	}{
		// Known mappings stay the same.
		{"claude-sonnet-4.5", "claude-sonnet-4.5", false},
		{"claude-opus-4.7", "claude-opus-4.7", false},
		{"gpt-4o", "claude-sonnet-4.5", false},
		{"gpt-4", "claude-sonnet-4.5", false},

		// Thinking suffix is preserved across the fallback.
		{"claude-opus-4.7-thinking", "claude-opus-4.7", true},

		// Pure Kiro non-claude models pass through.
		{"deepseek-3.2", "deepseek-3.2", false},
		{"qwen3-coder-next", "qwen3-coder-next", false},
		{"glm-5", "glm-5", false},
		{"minimax-m2.5", "minimax-m2.5", false},
		{"auto", "auto", false},

		// Unknown OpenAI / Codex model names fall back to the default
		// Sonnet so the account pool can still route the request.
		{"gpt-5", "claude-sonnet-4.5", false},
		{"gpt-5-codex", "claude-sonnet-4.5", false},
		{"o3-mini", "claude-sonnet-4.5", false},
		{"gpt-5-codex-thinking", "claude-sonnet-4.5", true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			gotModel, gotThinking := ParseModelAndThinking(tc.input, "-thinking")
			if gotModel != tc.wantModel {
				t.Fatalf("ParseModelAndThinking(%q) model = %q, want %q", tc.input, gotModel, tc.wantModel)
			}
			if gotThinking != tc.wantThinking {
				t.Fatalf("ParseModelAndThinking(%q) thinking = %v, want %v", tc.input, gotThinking, tc.wantThinking)
			}
		})
	}
}

func TestNormalizeChatRole(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"user", "user"},
		{"assistant", "assistant"},
		{"system", "system"},
		{"tool", "tool"},
		{"USER", "user"},
		{"Assistant", "assistant"},
		{"  user  ", "user"},
		{"developer", "system"},
		{"DEVELOPER", "system"},
		{"function", "tool"},
		{"FUNCTION", "tool"},
		{"", "user"},
		{"model", "user"},
		{"participant-1", "user"},
		{"unknown_role", "user"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := normalizeChatRole(tc.input)
			if got != tc.want {
				t.Fatalf("normalizeChatRole(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestOpenAIToKiroDeveloperRole(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "developer", Content: "you are helpful"},
			{Role: "user", Content: "hello"},
		},
	}
	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "you are helpful") {
		t.Fatalf("expected developer content merged as system prompt, got %q", cur.Content)
	}
}

func TestOpenAIToKiroUnknownRoleBecomesUser(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "model", Content: "I am a model response"},
			{Role: "user", Content: "follow up"},
		},
	}
	payload := OpenAIToKiro(req, false)
	if len(payload.ConversationState.History) != 1 {
		t.Fatalf("expected 1 history item, got %d", len(payload.ConversationState.History))
	}
	if payload.ConversationState.History[0].UserInputMessage == nil {
		t.Fatalf("expected unknown role to become user message in history")
	}
	if !strings.Contains(payload.ConversationState.History[0].UserInputMessage.Content, "I am a model response") {
		t.Fatalf("expected unknown role content in user message, got %q",
			payload.ConversationState.History[0].UserInputMessage.Content)
	}
}

func TestOpenAIToKiroUnknownRoleWithToolCallID(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "call tool"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "get_weather", Arguments: "{}"}}}},
			{Role: "unknown_role", ToolCallID: "call_1", Content: "sunny"},
		},
	}
	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "sunny") {
		t.Fatalf("expected unknown role with tool_call_id to become tool result, got %q", cur.Content)
	}
}

func TestClaudeToKiroUnknownRole(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "participant", Content: "custom role message"},
			{Role: "user", Content: "real user message"},
		},
	}
	payload := ClaudeToKiro(req, false)
	if len(payload.ConversationState.History) != 1 {
		t.Fatalf("expected 1 history item, got %d", len(payload.ConversationState.History))
	}
	if payload.ConversationState.History[0].UserInputMessage == nil {
		t.Fatalf("expected unknown Claude role to become user message in history")
	}
	if !strings.Contains(payload.ConversationState.History[0].UserInputMessage.Content, "custom role message") {
		t.Fatalf("expected unknown role content in user message, got %q",
			payload.ConversationState.History[0].UserInputMessage.Content)
	}
}
