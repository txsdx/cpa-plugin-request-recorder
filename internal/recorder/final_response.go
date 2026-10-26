package recorder

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FinalResponse keeps a provider's final JSON snapshot when available. Protocols
// with only deltas use a single assembled message snapshot; no SSE frames remain.
func FinalResponse(raw string) (string, bool) {
	if raw == "" || decode(raw) != nil || !strings.Contains(raw, "data:") {
		return raw, true
	}
	var snapshot map[string]any
	for _, event := range events(raw) {
		switch str(event["type"]) {
		case "response.completed", "response.incomplete", "response.failed":
			if value := obj(event["response"]); value != nil {
				snapshot = value
			}
		}
		// A complete Chat Completions message is also a terminal snapshot.
		for _, value := range arr(event["choices"]) {
			if message := obj(obj(value)["message"]); message != nil && (message["content"] != nil || message["tool_calls"] != nil || message["reasoning_content"] != nil) {
				snapshot = event
			}
		}
	}
	if snapshot != nil {
		data, _ := json.Marshal(snapshot)
		return string(data), true
	}
	messages := responseMessages(raw)
	complete := len(messages) > 0
	if !complete {
		messages = []Message{message("system", "error", "无法解析该提供商的流式输出；未保存 SSE 中间事件。")}
	}
	responseID := ""
	for _, event := range events(raw) {
		if value := str(event["id"]); value != "" {
			responseID = value
		}
		if value := str(obj(event["message"])["id"]); value != "" {
			responseID = value
		}
	}
	data, _ := json.Marshal(map[string]any{"object": "recorder.final_response", "id": responseID, "messages": messages})
	return string(data), complete
}

func previewText(messages []Message, role string) string {
	var parts []string
	hasText := false
	for _, m := range messages {
		if m.Role == role && m.Kind == "text" && m.Content != "" {
			hasText = true
		}
	}
	for _, m := range messages {
		if m.Role != role || m.Kind == "tool_result" {
			continue
		}
		if m.Kind == "text" {
			parts = append(parts, m.Content)
		} else if len(m.Images) > 0 {
			parts = append(parts, fmt.Sprintf("[原图 ×%d]", len(m.Images)))
		} else if m.Kind == "tool_call" {
			parts = append(parts, "调用工具 "+m.Name+": "+m.Content)
		} else if m.Kind == "reasoning" && !hasText && len(parts) == 0 {
			parts = append(parts, "[思考] "+m.Content)
		}
	}
	if len(parts) == 0 && role == "assistant" {
		for _, m := range messages {
			if m.Kind == "error" {
				parts = append(parts, "[错误] "+m.Content)
			}
		}
	}
	text := strings.Join(parts, "\n")
	chars := []rune(text)
	if len(chars) > 800 {
		text = string(chars[:800]) + "…"
	}
	return text
}
func updatePreviews(r *Record) {
	// Retain only the latest user turn, grouping its text and image blocks.
	request := requestMessages(r.RequestBody)
	start := -1
	for i, m := range request {
		if m.Role == "user" && m.Kind != "tool_result" {
			if i == 0 || request[i-1].Role != "user" {
				start = i
			}
		}
	}
	if start >= 0 {
		r.LatestPrompt = previewText(request[start:], "user")
	}
	r.LatestResponse = previewText(responseMessages(r.ResponseBody), "assistant")
	r.LatestResponseSource = "cpa_response"
	if r.UpstreamResponses != "" {
		if value := previewText(responseMessages(r.UpstreamResponses), "assistant"); value != "" {
			r.LatestResponse = value
			r.LatestResponseSource = "provider_response"
		}
	}
}
