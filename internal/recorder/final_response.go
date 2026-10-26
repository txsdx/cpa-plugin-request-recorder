package recorder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Preserve the complete JSON response carried by one callback. The data: line
// has not necessarily received the HTTP writer's trailing SSE delimiters yet.
func callbackSnapshot(payload []byte) (string, bool) {
	payload = bytes.TrimSpace(payload)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[5:])
	}
	var event struct {
		Type     string          `json:"type"`
		Object   string          `json:"object"`
		Response json.RawMessage `json:"response"`
		Choices  []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return "", false
	}
	if isResponseTerminal(event.Type) {
		var response struct {
			Output []json.RawMessage `json:"output"`
		}
		if json.Unmarshal(event.Response, &response) == nil && len(response.Output) > 0 {
			return string(event.Response), true
		}
	}
	if event.Type == "" && event.Object == "response" {
		var response struct {
			Output []json.RawMessage `json:"output"`
		}
		if json.Unmarshal(payload, &response) == nil && len(response.Output) > 0 {
			return string(payload), true
		}
	}
	for _, choice := range event.Choices {
		if len(choice.Message) > 0 && string(choice.Message) != "null" {
			return string(payload), true
		}
	}
	return "", false
}
func isResponseTerminal(typ string) bool {
	return typ == "response.completed" || typ == "response.done" || typ == "response.incomplete" || typ == "response.failed"
}

// FinalResponse keeps a provider's final JSON snapshot when available. Protocols
// with only deltas use a single assembled message snapshot; no SSE frames remain.
func FinalResponse(raw string) (string, bool) {
	if snapshot, ok := callbackSnapshot([]byte(raw)); ok {
		return snapshot, true
	}
	if raw == "" || decode(raw) != nil || !strings.Contains(raw, "data:") {
		return raw, true
	}
	var snapshot map[string]any
	ev := events(raw)
	items := map[int]any{}
	for _, event := range ev {
		if str(event["type"]) == "response.output_item.done" && obj(event["item"]) != nil {
			index := num(event["output_index"])
			if _, exists := event["output_index"]; !exists {
				index = len(items)
			}
			items[index] = event["item"]
		}
		switch str(event["type"]) {
		case "response.completed", "response.done", "response.incomplete", "response.failed":
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
		if len(arr(snapshot["output"])) > 0 || len(fullResponse(snapshot)) > 0 || len(items) > 0 || len(ev) == 1 {
			if len(arr(snapshot["output"])) == 0 && len(items) > 0 {
				indexes := make([]int, 0, len(items))
				for index := range items {
					indexes = append(indexes, index)
				}
				sort.Ints(indexes)
				output := make([]any, 0, len(indexes))
				for _, index := range indexes {
					output = append(output, items[index])
				}
				snapshot["output"] = output
			}
			data, _ := json.Marshal(snapshot)
			return string(data), true
		}
	}
	messages := responseMessages(raw)
	complete := len(ev) > 0 && len(messages) > 0
	if !complete {
		messages = []Message{message("system", "error", "无法解析该提供商的流式输出；未保存 SSE 中间事件。")}
	}
	responseID := ""
	for _, event := range ev {
		if value := str(event["id"]); value != "" {
			responseID = value
		}
		if value := str(obj(event["message"])["id"]); value != "" {
			responseID = value
		}
		if value := str(obj(event["response"])["id"]); value != "" {
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
}
