package recorder

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type Message struct {
	Role       string  `json:"role"`
	Kind       string  `json:"kind"`
	Name       string  `json:"name,omitempty"`
	ToolCallID string  `json:"tool_call_id,omitempty"`
	Content    string  `json:"content"`
	Choice     int     `json:"choice,omitempty"`
	Source     string  `json:"source"`
	Images     []Image `json:"images,omitempty"`
}
type Image struct {
	URL string `json:"url"`
	Alt string `json:"alt"`
}

func imageMessage(role string, b map[string]any) Message {
	m := message(role, "image", "")
	url := str(b["url"])
	if v := b["image_url"]; v != nil {
		if s, ok := v.(string); ok {
			url = s
		} else {
			url = str(obj(v)["url"])
		}
	}
	if source := obj(b["source"]); source != nil {
		if str(source["type"]) == "base64" {
			url = "data:" + str(source["media_type"]) + ";base64," + str(source["data"])
		} else {
			url = str(source["url"])
		}
	}
	for _, key := range []string{"inlineData", "inline_data"} {
		if source := obj(b[key]); source != nil {
			mime := str(source["mimeType"])
			if mime == "" {
				mime = str(source["mime_type"])
			}
			url = "data:" + mime + ";base64," + str(source["data"])
		}
	}
	if encoded := str(b["b64_json"]); encoded != "" {
		mime := str(b["mime_type"])
		if mime == "" {
			mime = "image/png"
		}
		url = "data:" + mime + ";base64," + encoded
	}
	if str(b["type"]) == "image_generation_call" {
		mime := "image/png"
		switch str(b["output_format"]) {
		case "jpeg":
			mime = "image/jpeg"
		case "webp":
			mime = "image/webp"
		}
		url = "data:" + mime + ";base64," + str(b["result"])
	}
	if url != "" {
		m.Images = []Image{{URL: url, Alt: "原始图片"}}
	} else {
		m.Content = printable(b)
	}
	return m
}
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func arr(v any) []any          { a, _ := v.([]any); return a }
func num(v any) int            { f, _ := v.(float64); return int(f) }
func printable(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
func decode(s string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}
func message(role, kind, content string) Message {
	return Message{Role: role, Kind: kind, Content: content}
}
func blocks(role string, content any) []Message {
	out := []Message{}
	if s, ok := content.(string); ok {
		if s != "" {
			out = append(out, message(role, "text", s))
		}
		return out
	}
	for _, v := range arr(content) {
		b := obj(v)
		t := str(b["type"])
		switch t {
		case "text", "input_text", "output_text":
			out = append(out, message(role, "text", str(b["text"])))
		case "thinking", "reasoning":
			s := str(b["thinking"])
			if s == "" {
				s = str(b["text"])
			}
			if s == "" && b["thinking"] == nil && b["text"] == nil {
				s = printable(b)
			}
			out = append(out, message(role, "reasoning", s))
		case "redacted_thinking":
			out = append(out, message(role, "reasoning", "[上游隐藏的思考内容]"))
		case "tool_use":
			m := message("assistant", "tool_call", printable(b["input"]))
			m.Name = str(b["name"])
			m.ToolCallID = str(b["id"])
			out = append(out, m)
		case "tool_result":
			m := message("tool", "tool_result", contentText(b["content"]))
			m.ToolCallID = str(b["tool_use_id"])
			for _, part := range blocks("tool", b["content"]) {
				m.Images = append(m.Images, part.Images...)
			}
			out = append(out, m)
		case "image", "image_url", "input_image", "output_image":
			out = append(out, imageMessage(role, b))
		case "input_audio", "audio", "file", "document", "input_file":
			out = append(out, message(role, "attachment", printable(b)))
		default:
			out = append(out, message(role, "structured", printable(b)))
		}
	}
	return out
}
func contentText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	parts := blocks("tool", v)
	if len(parts) == 0 {
		return printable(v)
	}
	a := []string{}
	for _, p := range parts {
		a = append(a, p.Content)
	}
	return strings.Join(a, "\n")
}
func chatMessage(m map[string]any) []Message {
	role := str(m["role"])
	if role == "" {
		role = "user"
	}
	out := blocks(role, m["content"])
	if role == "tool" {
		for i := range out {
			out[i].Kind = "tool_result"
			out[i].ToolCallID = str(m["tool_call_id"])
			out[i].Name = str(m["name"])
		}
	}
	for _, k := range []string{"reasoning_content", "reasoning"} {
		if s := str(m[k]); s != "" {
			out = append([]Message{message(role, "reasoning", s)}, out...)
		}
	}
	calls := arr(m["tool_calls"])
	if fc := obj(m["function_call"]); fc != nil {
		calls = append(calls, map[string]any{"function": fc})
	}
	for _, v := range calls {
		c := obj(v)
		f := obj(c["function"])
		t := message("assistant", "tool_call", printable(f["arguments"]))
		t.Name = str(f["name"])
		t.ToolCallID = str(c["id"])
		out = append(out, t)
	}
	return out
}
func responseItem(m map[string]any) []Message {
	switch str(m["type"]) {
	case "image_generation_call":
		return []Message{imageMessage("assistant", m)}
	case "function_call":
		t := message("assistant", "tool_call", printable(m["arguments"]))
		t.Name = str(m["name"])
		t.ToolCallID = str(m["call_id"])
		return []Message{t}
	case "function_call_output":
		t := message("tool", "tool_result", printable(m["output"]))
		t.ToolCallID = str(m["call_id"])
		return []Message{t}
	case "reasoning":
		out := []Message{}
		for _, v := range arr(m["summary"]) {
			out = append(out, message("assistant", "reasoning", str(obj(v)["text"])))
		}
		if len(out) == 0 {
			out = append(out, message("assistant", "reasoning", printable(m)))
		}
		return out
	default:
		return chatMessage(m)
	}
}
func geminiContent(m map[string]any) []Message {
	role := str(m["role"])
	if role == "model" {
		role = "assistant"
	}
	if role == "" {
		role = "user"
	}
	out := []Message{}
	for _, v := range arr(m["parts"]) {
		p := obj(v)
		if s, ok := p["text"].(string); ok {
			k := "text"
			if p["thought"] == true {
				k = "reasoning"
			}
			out = append(out, message(role, k, s))
		}
		if f := obj(p["functionCall"]); f != nil {
			t := message("assistant", "tool_call", printable(f["args"]))
			t.Name = str(f["name"])
			t.ToolCallID = str(f["id"])
			out = append(out, t)
		}
		if f := obj(p["functionResponse"]); f != nil {
			t := message("tool", "tool_result", printable(f["response"]))
			t.Name = str(f["name"])
			t.ToolCallID = str(f["id"])
			out = append(out, t)
		}
		if p["inlineData"] != nil || p["inline_data"] != nil {
			src := obj(p["inlineData"])
			if src == nil {
				src = obj(p["inline_data"])
			}
			mime := str(src["mimeType"]) + str(src["mime_type"])
			if strings.HasPrefix(mime, "image/") {
				out = append(out, imageMessage(role, p))
			} else {
				out = append(out, message(role, "attachment", printable(p)))
			}
		}
		if p["fileData"] != nil || p["file_data"] != nil {
			src := obj(p["fileData"])
			if src == nil {
				src = obj(p["file_data"])
			}
			mime := str(src["mimeType"]) + str(src["mime_type"])
			url := str(src["fileUri"])
			if url == "" {
				url = str(src["file_uri"])
			}
			if strings.HasPrefix(mime, "image/") {
				out = append(out, imageMessage(role, map[string]any{"url": url}))
			} else {
				out = append(out, message(role, "attachment", printable(p)))
			}
		}
	}
	return out
}
func requestMessages(raw string) []Message {
	m := decode(raw)
	if req := obj(m["request"]); req != nil {
		m = req
	}
	out := []Message{}
	if m == nil {
		if raw != "" {
			out = append(out, message("system", "structured", "无法解析原请求，请查看原文"))
		}
		return out
	}
	if v := m["system"]; v != nil {
		out = append(out, blocks("system", v)...)
	}
	if v := m["instructions"]; v != nil {
		out = append(out, blocks("system", v)...)
	}
	for _, key := range []string{"systemInstruction", "system_instruction"} {
		if v := obj(m[key]); v != nil {
			p := geminiContent(v)
			for i := range p {
				p[i].Role = "system"
			}
			out = append(out, p...)
		}
	}
	for _, v := range arr(m["messages"]) {
		out = append(out, chatMessage(obj(v))...)
	}
	if v, ok := m["input"].(string); ok {
		out = append(out, message("user", "text", v))
	} else {
		for _, v := range arr(m["input"]) {
			out = append(out, responseItem(obj(v))...)
		}
	}
	for _, v := range arr(m["contents"]) {
		out = append(out, geminiContent(obj(v))...)
	}
	if v := m["prompt"]; v != nil {
		out = append(out, message("user", "text", printable(v)))
	}
	if str(m["previous_response_id"]) != "" {
		out = append([]Message{message("system", "notice", "请求使用 previous_response_id；前置历史未包含在本请求中，不能凭空还原。")}, out...)
	}
	return out
}
func fullResponse(m map[string]any) []Message {
	out := []Message{}
	for _, v := range arr(m["data"]) {
		b := obj(v)
		if b["b64_json"] != nil || b["url"] != nil {
			out = append(out, imageMessage("assistant", b))
		}
	}
	for _, v := range arr(m["choices"]) {
		c := obj(v)
		var a []Message
		if mm := obj(c["message"]); mm != nil {
			a = chatMessage(mm)
		} else if text := str(c["text"]); text != "" {
			a = []Message{message("assistant", "text", text)}
		}
		for i := range a {
			a[i].Choice = num(c["index"])
		}
		out = append(out, a...)
	}
	for _, v := range arr(m["output"]) {
		out = append(out, responseItem(obj(v))...)
	}
	if m["content"] != nil {
		out = append(out, blocks("assistant", m["content"])...)
	}
	for _, v := range arr(m["candidates"]) {
		c := obj(v)
		a := geminiContent(obj(c["content"]))
		for i := range a {
			a[i].Choice = num(c["index"])
		}
		out = append(out, a...)
	}
	if m["error"] != nil {
		out = append(out, message("system", "error", printable(m["error"])))
	}
	return out
}
func events(raw string) []map[string]any {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	out := []map[string]any{}
	for _, frame := range strings.Split(raw, "\n\n") {
		data := []string{}
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		joined := strings.Join(data, "\n")
		if joined == "" || joined == "[DONE]" {
			continue
		}
		if m := decode(joined); m != nil {
			out = append(out, m)
		}
	}
	return out
}

type streamBuilder struct {
	msgs  map[string]*Message
	order []string
}

func (b *streamBuilder) slot(key, role, kind string) *Message {
	if m := b.msgs[key]; m != nil {
		return m
	}
	m := message(role, kind, "")
	b.msgs[key] = &m
	b.order = append(b.order, key)
	return &m
}
func responseMessages(raw string) []Message {
	if m := decode(raw); m != nil {
		return fullResponse(m)
	}
	ev := events(raw)
	if len(ev) == 0 {
		if raw != "" {
			return []Message{message("system", "structured", "未识别响应格式；完整内容保留在原文页签。")}
		}
		return nil
	}
	b := streamBuilder{msgs: map[string]*Message{}}
	var terminal []Message
	for _, m := range ev {
		typ := str(m["type"])
		if (typ == "response.completed" || typ == "response.incomplete" || typ == "response.failed") && obj(m["response"]) != nil {
			terminal = fullResponse(obj(m["response"]))
			if typ != "response.completed" {
				terminal = append(terminal, message("system", "notice", typ))
			}
			continue
		}
		for _, v := range arr(m["choices"]) {
			c := obj(v)
			choice := num(c["index"])
			prefix := fmt.Sprint("choice:", choice)
			d := obj(c["delta"])
			if d == nil {
				d = obj(c["message"])
			}
			for i, part := range blocks("assistant", arr(d["content"])) {
				a := b.slot(prefix+"part:"+strconv.Itoa(i)+":"+part.Kind, part.Role, part.Kind)
				a.Content += part.Content
				a.Images = append(a.Images, part.Images...)
				a.Choice = choice
			}
			for _, k := range []string{"content", "reasoning_content", "reasoning"} {
				if s := str(d[k]); s != "" {
					kind := "text"
					if k != "content" {
						kind = "reasoning"
					}
					a := b.slot(prefix+kind, "assistant", kind)
					a.Content += s
					a.Choice = choice
				}
			}
			if s := str(c["text"]); s != "" {
				a := b.slot(prefix+"text", "assistant", "text")
				a.Content += s
				a.Choice = choice
			}
			calls := arr(d["tool_calls"])
			if fc := obj(d["function_call"]); fc != nil {
				calls = append(calls, map[string]any{"index": float64(0), "function": fc})
			}
			for _, v := range calls {
				t := obj(v)
				f := obj(t["function"])
				a := b.slot(prefix+"tool:"+fmt.Sprint(num(t["index"])), "assistant", "tool_call")
				a.Name += str(f["name"])
				a.ToolCallID += str(t["id"])
				a.Content += str(f["arguments"])
				a.Choice = choice
			}
		}
		idx := fmt.Sprint(num(m["index"]))
		if typ == "content_block_start" {
			block := obj(m["content_block"])
			for _, a := range blocks("assistant", []any{block}) {
				slot := b.slot("claude:"+idx, a.Role, a.Kind)
				*slot = a
				if a.Kind == "tool_call" && len(obj(block["input"])) == 0 {
					slot.Content = ""
				}
			}
		}
		if typ == "content_block_delta" {
			d := obj(m["delta"])
			kind := "text"
			text := str(d["text"])
			switch str(d["type"]) {
			case "thinking_delta":
				kind = "reasoning"
				text = str(d["thinking"])
			case "input_json_delta":
				kind = "tool_call"
				text = str(d["partial_json"])
			case "signature_delta":
				continue
			}
			a := b.slot("claude:"+idx, "assistant", kind)
			a.Content += text
		}
		// Responses events use output_index to keep concurrent tool calls distinct.
		ri := "responses:" + fmt.Sprint(num(m["output_index"]))
		switch typ {
		case "image_generation.completed":
			a := b.slot("image-generation", "assistant", "image")
			*a = imageMessage("assistant", m)
		case "response.output_item.added":
			item := obj(m["item"])
			if str(item["type"]) == "function_call" {
				a := b.slot(ri+"tool", "assistant", "tool_call")
				a.Name = str(item["name"])
				a.ToolCallID = str(item["call_id"])
				a.Content = str(item["arguments"])
			}
		case "response.function_call_arguments.delta":
			b.slot(ri+"tool", "assistant", "tool_call").Content += str(m["delta"])
		case "response.output_text.delta":
			b.slot(ri+"text", "assistant", "text").Content += str(m["delta"])
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			b.slot(ri+"reasoning", "assistant", "reasoning").Content += str(m["delta"])
		case "error":
			a := b.slot("error", "system", "error")
			a.Content = printable(m)
		}
		if typ == "response.image_generation_call.partial_image" {
			a := b.slot(ri+"image", "assistant", "image")
			*a = imageMessage("assistant", map[string]any{"b64_json": m["partial_image_b64"]})
		}
		for _, v := range arr(m["candidates"]) {
			c := obj(v)
			for j, a := range geminiContent(obj(c["content"])) {
				key := "gemini:" + fmt.Sprint(num(c["index"])) + ":" + strconv.Itoa(j) + ":" + a.Kind
				slot := b.slot(key, a.Role, a.Kind)
				slot.Content += a.Content
				slot.Name = a.Name
				slot.ToolCallID = a.ToolCallID
				slot.Images = append(slot.Images, a.Images...)
			}
		}
	}
	if len(terminal) > 0 {
		return terminal
	}
	out := []Message{}
	for _, k := range b.order {
		out = append(out, *b.msgs[k])
	}
	return out
}
func Conversation(request, response string) []Message {
	out := requestMessages(request)
	for i := range out {
		out[i].Source = "request"
	}
	resp := responseMessages(response)
	for i := range resp {
		resp[i].Source = "response"
	}
	out = append(out, resp...)
	names := map[string]string{}
	for _, m := range out {
		if m.Kind == "tool_call" && m.ToolCallID != "" {
			names[m.ToolCallID] = m.Name
		}
	}
	for i := range out {
		if out[i].Kind == "tool_result" && out[i].Name == "" {
			out[i].Name = names[out[i].ToolCallID]
		}
	}
	return out
}
func UpstreamConversation(r *Record) []Message {
	if r.UpstreamResponses == "" {
		return nil
	}
	// Select the last attempt by its numeric index, while preserving all attempts
	// in the raw pane. Headers and timestamps remain outside the protocol parser.
	type attempt struct {
		index int
		body  string
	}
	parts := []attempt{}
	locs := sectionPattern.FindAllStringSubmatchIndex(r.UpstreamResponses, -1)
	for i, loc := range locs {
		end := len(r.UpstreamResponses)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		name := r.UpstreamResponses[loc[2]:loc[3]]
		index := 0
		_, _ = fmt.Sscanf(name, "API RESPONSE %d", &index)
		body := r.UpstreamResponses[loc[1]:end]
		if _, v, ok := strings.Cut(body, "Body:\n"); ok {
			parts = append(parts, attempt{index, strings.TrimSpace(v)})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].index < parts[j].index })
	msgs := responseMessages(parts[len(parts)-1].body)
	for i := range msgs {
		msgs[i].Source = "upstream"
	}
	return msgs
}
