package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// handleResponses serves POST /v1/responses for Codex / OpenAI Responses clients.
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendResponsesError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendResponsesError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		h.sendResponsesError(w, 400, "invalid_request_error", "model is required")
		return
	}
	if req.Input == nil {
		h.sendResponsesError(w, 400, "invalid_request_error", "input is required")
		return
	}

	// Translate to the internal Claude-style request first; reuse the same
	// tooling, validation, and Kiro-payload assembly that powers /v1/messages.
	claudeReq := responsesToClaudeRequest(&req)
	if msg := validateClaudeRequestShape(claudeReq); msg != "" {
		h.sendResponsesError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, suffixThinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	thinking := suffixThinking || reasoningEffortRequestsThinking(req.Reasoning)
	claudeReq.Model = actualModel

	account := h.pool.GetNextForModel(actualModel)
	if account == nil {
		h.sendResponsesError(w, 503, "api_error", "No available accounts")
		return
	}
	if err := h.ensureValidToken(account); err != nil {
		h.sendResponsesError(w, 503, "api_error", "Token refresh failed: "+err.Error())
		return
	}

	effectiveReq := cloneClaudeRequestForThinking(claudeReq, thinking)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	kiroPayload := ClaudeToKiro(claudeReq, thinking)

	if rc := getRequestCtx(r.Context()); rc != nil {
		rc.model = req.Model
		rc.actualModel = actualModel
		rc.streaming = req.Stream
		rc.accountID = account.ID
		rc.accountEmail = account.Email
	}

	if req.Stream {
		h.handleResponsesStream(r.Context(), w, r, account, kiroPayload, req.Model, thinking, estimatedInputTokens)
	} else {
		h.handleResponsesNonStream(r.Context(), w, account, kiroPayload, req.Model, thinking, estimatedInputTokens)
	}
}

func (h *Handler) sendResponsesError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// ====================== Non-streaming response ======================

func (h *Handler) handleResponsesNonStream(ctx context.Context, w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int) {
	var content string
	var thinkingContent string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				thinkingContent += text
			} else {
				content += text
			}
		},
		OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
		OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
		OnError:    func(err error) { h.pool.RecordError(account.ID, strings.Contains(err.Error(), "429")) },
		OnCredits:  func(c float64) { credits = c },
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
	}

	if err := CallKiroAPI(account, payload, callback); err != nil {
		h.recordFailure()
		h.pool.RecordError(account.ID, strings.Contains(err.Error(), "429"))
		h.checkOverageError(err, account.ID)
		h.sendResponsesError(w, 500, "api_error", err.Error())
		return
	}

	finalContent, extractedReasoning := extractThinkingFromContent(content)
	if thinking && thinkingContent == "" && extractedReasoning != "" {
		thinkingContent = extractedReasoning
	}
	if !thinking {
		thinkingContent = ""
	}

	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputTokens = estimateClaudeOutputTokens(finalContent, thinkingContent, toolUses)

	h.recordSuccessForRequest(ctx, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

	resp := buildResponsesResponse(model, finalContent, thinkingContent, toolUses, inputTokens, outputTokens)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func buildResponsesResponse(model, content, thinkingContent string, toolUses []KiroToolUse, inputTokens, outputTokens int) *ResponsesResponse {
	resp := &ResponsesResponse{
		ID:        newResponseID(),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     model,
		Output:    make([]ResponsesOutputItem, 0, 3),
		Usage: ResponsesUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		},
	}

	if thinkingContent != "" {
		resp.Output = append(resp.Output, ResponsesOutputItem{
			Type:   "reasoning",
			ID:     newOutputItemID("rsng"),
			Status: "completed",
			Summary: []ResponsesOutputContentPart{
				{Type: "summary_text", Text: thinkingContent},
			},
		})
	}

	if content != "" {
		resp.Output = append(resp.Output, ResponsesOutputItem{
			Type:   "message",
			ID:     newOutputItemID("msg"),
			Status: "completed",
			Role:   "assistant",
			Content: []ResponsesOutputContentPart{
				{Type: "output_text", Text: content},
			},
		})
		resp.OutputText = content
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		resp.Output = append(resp.Output, ResponsesOutputItem{
			Type:      "function_call",
			ID:        newOutputItemID("fc"),
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	return resp
}

// ====================== Streaming response ======================

func (h *Handler) handleResponsesStream(ctx context.Context, w http.ResponseWriter, r *http.Request, account *config.Account, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy/nginx buffering for SSE

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendResponsesError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// ctx is provided by handleResponses (already wrapped with the request
	// log context). Falling back to r.Context() here would lose that.
	_ = r // keep r in signature so callers can still inspect headers later

	respID := newResponseID()
	createdAt := time.Now().Unix()
	sequenceNumber := 0
	// emit returns the error from the underlying Write so the caller can
	// abort cleanly when the client has hung up. sequence_number only
	// advances after the bytes have been successfully written to the client,
	// otherwise the client (which validates monotonic sequencing) would
	// observe gaps after a partial write.
	emit := func(event string, data map[string]interface{}) error {
		data["type"] = event
		data["sequence_number"] = sequenceNumber
		raw, err := json.Marshal(data)
		if err != nil {
			logger.Warnf("[Responses] marshal %s failed: %v", event, err)
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(raw)); err != nil {
			return err
		}
		flusher.Flush()
		sequenceNumber++
		return nil
	}

	// State for streaming text/reasoning/tool blocks.
	type activeItem struct {
		ID      string
		Type    string // "message" | "reasoning"
		Index   int
		Content strings.Builder
	}
	var current *activeItem
	outputIndex := 0
	finalOutput := make([]ResponsesOutputItem, 0)
	clientGone := false

	// Make every emit a no-op once the client has gone away or panicked, so
	// later defer-driven cleanup paths don't double up errors.
	safeEmit := func(event string, data map[string]interface{}) {
		if clientGone {
			return
		}
		// If the client cancelled the request (Ctrl+C in Codex, network
		// drop, etc.) stop emitting; we'd otherwise burn cycles writing to
		// a closed connection and risk blocking on TCP back-pressure.
		select {
		case <-ctx.Done():
			clientGone = true
			return
		default:
		}
		if err := emit(event, data); err != nil {
			clientGone = true
			logger.Warnf("[Responses] client write failed during %s: %v", event, err)
		}
	}

	// Defensive recovery: a panic anywhere in the stream goroutine would
	// otherwise leave the client hanging on the open SSE socket forever.
	// Codex CLI's SSE parser treats response.failed as a terminal event, so
	// we don't need to follow it with response.completed (doing so actually
	// confuses the client). We just emit response.failed and return.
	defer func() {
		if rec := recover(); rec != nil {
			logger.Errorf("[Responses] panic in stream handler: %v", rec)
			safeEmit("response.failed", map[string]interface{}{
				"response": map[string]interface{}{
					"id":         respID,
					"object":     "response",
					"created_at": createdAt,
					"status":     "failed",
					"model":      model,
					"error": map[string]string{
						"code":    "internal_error",
						"message": fmt.Sprintf("internal error: %v", rec),
					},
				},
			})
		}
	}()

	safeEmit("response.created", map[string]interface{}{
		"response": map[string]interface{}{
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      model,
			"output":     []interface{}{},
		},
	})
	safeEmit("response.in_progress", map[string]interface{}{
		"response": map[string]interface{}{
			"id":     respID,
			"status": "in_progress",
		},
	})

	closeActiveItem := func() {
		if current == nil {
			return
		}
		if current.Type == "message" {
			text := current.Content.String()
			safeEmit("response.output_text.done", map[string]interface{}{
				"item_id":       current.ID,
				"output_index":  current.Index,
				"content_index": 0,
				"text":          text,
				"logprobs":      []interface{}{},
			})
			safeEmit("response.content_part.done", map[string]interface{}{
				"item_id":       current.ID,
				"output_index":  current.Index,
				"content_index": 0,
				"part": map[string]interface{}{
					"type":        "output_text",
					"text":        text,
					"annotations": []interface{}{},
				},
			})
			safeEmit("response.output_item.done", map[string]interface{}{
				"output_index": current.Index,
				"item": map[string]interface{}{
					"id":     current.ID,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"content": []map[string]interface{}{{
						"type":        "output_text",
						"text":        text,
						"annotations": []interface{}{},
					}},
				},
			})
			finalOutput = append(finalOutput, ResponsesOutputItem{
				Type:   "message",
				ID:     current.ID,
				Status: "completed",
				Role:   "assistant",
				Content: []ResponsesOutputContentPart{
					{Type: "output_text", Text: text},
				},
			})
		} else if current.Type == "reasoning" {
			text := current.Content.String()
			safeEmit("response.reasoning_summary_text.done", map[string]interface{}{
				"item_id":       current.ID,
				"output_index":  current.Index,
				"summary_index": 0,
				"text":          text,
			})
			safeEmit("response.output_item.done", map[string]interface{}{
				"output_index": current.Index,
				"item": map[string]interface{}{
					"id":     current.ID,
					"type":   "reasoning",
					"status": "completed",
					"summary": []map[string]interface{}{{
						"type": "summary_text",
						"text": text,
					}},
					"encrypted_content": nil,
				},
			})
			finalOutput = append(finalOutput, ResponsesOutputItem{
				Type:   "reasoning",
				ID:     current.ID,
				Status: "completed",
				Summary: []ResponsesOutputContentPart{
					{Type: "summary_text", Text: text},
				},
			})
		}
		current = nil
	}

	openItem := func(itemType string) {
		if current != nil && current.Type == itemType {
			return
		}
		closeActiveItem()
		idPrefix := "msg"
		if itemType == "reasoning" {
			idPrefix = "rsng"
		}
		id := newOutputItemID(idPrefix)
		current = &activeItem{ID: id, Type: itemType, Index: outputIndex}
		outputIndex++

		switch itemType {
		case "message":
			safeEmit("response.output_item.added", map[string]interface{}{
				"output_index": current.Index,
				"item": map[string]interface{}{
					"id":      id,
					"type":    "message",
					"status":  "in_progress",
					"role":    "assistant",
					"content": []interface{}{},
				},
			})
			safeEmit("response.content_part.added", map[string]interface{}{
				"item_id":       id,
				"output_index":  current.Index,
				"content_index": 0,
				"part": map[string]interface{}{
					"type":        "output_text",
					"text":        "",
					"annotations": []interface{}{},
				},
			})
		case "reasoning":
			safeEmit("response.output_item.added", map[string]interface{}{
				"output_index": current.Index,
				"item": map[string]interface{}{
					"id":                id,
					"type":              "reasoning",
					"status":            "in_progress",
					"summary":           []interface{}{},
					"encrypted_content": nil,
				},
			})
		}
	}

	emitText := func(delta string, isThinking bool) {
		if delta == "" {
			return
		}
		if isThinking {
			openItem("reasoning")
			current.Content.WriteString(delta)
			safeEmit("response.reasoning_summary_text.delta", map[string]interface{}{
				"item_id":       current.ID,
				"output_index":  current.Index,
				"summary_index": 0,
				"delta":         delta,
			})
			return
		}
		openItem("message")
		current.Content.WriteString(delta)
		safeEmit("response.output_text.delta", map[string]interface{}{
			"item_id":       current.ID,
			"output_index":  current.Index,
			"content_index": 0,
			"delta":         delta,
		})
	}

	emitToolCall := func(tu KiroToolUse) {
		closeActiveItem()
		argsBytes, _ := json.Marshal(tu.Input)
		args := string(argsBytes)
		itemID := "fc_" + uuid.New().String()
		idx := outputIndex
		outputIndex++

		safeEmit("response.output_item.added", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"id":        itemID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   tu.ToolUseID,
				"name":      tu.Name,
				"arguments": "",
			},
		})
		safeEmit("response.function_call_arguments.delta", map[string]interface{}{
			"item_id":      itemID,
			"output_index": idx,
			"delta":        args,
		})
		safeEmit("response.function_call_arguments.done", map[string]interface{}{
			"item_id":      itemID,
			"output_index": idx,
			"arguments":    args,
		})
		safeEmit("response.output_item.done", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"id":        itemID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   tu.ToolUseID,
				"name":      tu.Name,
				"arguments": args,
			},
		})
		finalOutput = append(finalOutput, ResponsesOutputItem{
			Type:      "function_call",
			ID:        itemID,
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: args,
		})
	}

	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var rawContentBuilder, rawThinkingBuilder strings.Builder
	var upstreamErr error

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if text == "" {
				return
			}
			if isThinking {
				rawThinkingBuilder.WriteString(text)
				if !thinking {
					return
				}
				emitText(text, true)
			} else {
				rawContentBuilder.WriteString(text)
				emitText(text, false)
			}
		},
		OnToolUse:  emitToolCall,
		OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
		OnError: func(err error) {
			logger.Warnf("[Responses] upstream error: %v", err)
			h.pool.RecordError(account.ID, strings.Contains(err.Error(), "429"))
		},
		OnCredits: func(c float64) { credits = c },
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
	}

	upstreamErr = CallKiroAPI(account, payload, callback)

	// Always close any in-flight block before sending the final event, even
	// on error, so the SSE message tree stays well-formed.
	closeActiveItem()

	if upstreamErr != nil {
		h.recordFailure()
		h.pool.RecordError(account.ID, strings.Contains(upstreamErr.Error(), "429"))
		h.checkOverageError(upstreamErr, account.ID)
		// Codex CLI treats response.failed as terminal �?once it arrives the
		// stream parser stops. We must not follow up with response.completed
		// because Codex returns on the first terminal event it sees and
		// would surface "completed" instead of the actual error.
		errCode := "api_error"
		if strings.Contains(upstreamErr.Error(), "429") {
			errCode = "rate_limit_exceeded"
		} else if strings.Contains(upstreamErr.Error(), "402") {
			errCode = "insufficient_quota"
		}
		safeEmit("response.failed", map[string]interface{}{
			"response": map[string]interface{}{
				"id":         respID,
				"object":     "response",
				"created_at": createdAt,
				"status":     "failed",
				"model":      model,
				"error": map[string]string{
					"code":    errCode,
					"message": upstreamErr.Error(),
				},
			},
		})
		return
	}

	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputContent, _ := extractThinkingFromContent(rawContentBuilder.String())
	thinkingOutput := rawThinkingBuilder.String()
	if !thinking {
		thinkingOutput = ""
	}
	outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, nil)

	// Some upstream responses produce no text and no tool calls (rare, but
	// happens when the model decides the turn is empty). Codex treats an
	// empty `output` array as a protocol error ("model returned no output");
	// emit a placeholder empty assistant message so the turn closes cleanly.
	if len(finalOutput) == 0 {
		emptyID := newOutputItemID("msg")
		idx := outputIndex
		outputIndex++
		safeEmit("response.output_item.added", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"id":      emptyID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []interface{}{},
			},
		})
		safeEmit("response.output_item.done", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"id":     emptyID,
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]interface{}{{
					"type":        "output_text",
					"text":        "",
					"annotations": []interface{}{},
				}},
			},
		})
		finalOutput = append(finalOutput, ResponsesOutputItem{
			Type:   "message",
			ID:     emptyID,
			Status: "completed",
			Role:   "assistant",
			Content: []ResponsesOutputContentPart{
				{Type: "output_text", Text: ""},
			},
		})
	}

	h.recordSuccessForRequest(ctx, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

	finalOutputJSON := make([]map[string]interface{}, 0, len(finalOutput))
	for _, item := range finalOutput {
		raw, _ := json.Marshal(item)
		var m map[string]interface{}
		_ = json.Unmarshal(raw, &m)
		finalOutputJSON = append(finalOutputJSON, m)
	}

	safeEmit("response.completed", map[string]interface{}{
		"response": map[string]interface{}{
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "completed",
			"model":      model,
			"output":     finalOutputJSON,
			"usage": map[string]interface{}{
				"input_tokens":  inputTokens,
				"input_tokens_details": map[string]int{
					"cached_tokens": 0,
				},
				"output_tokens": outputTokens,
				"output_tokens_details": map[string]int{
					"reasoning_tokens": 0,
				},
				"total_tokens": inputTokens + outputTokens,
			},
		},
	})
}
