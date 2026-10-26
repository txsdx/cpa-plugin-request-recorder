package recorder

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConversationToolsAndMultimodal(t *testing.T) {
	cases := []struct {
		name, req, res string
		want           []string
	}{
		{"chat", `{"messages":[{"role":"user","content":"天气？"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"杭州\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"晴天"}]}`, `{"choices":[{"message":{"role":"assistant","content":"今天晴天"}}]}`, []string{"天气？", "weather", "杭州", "晴天", "今天晴天"}},
		{"responses", `{"instructions":"be helpful","input":[{"type":"function_call","name":"read_file","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"contents"}]}`, `{"output":[{"type":"reasoning","summary":[{"text":"thinking"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`, []string{"be helpful", "read_file", "contents", "thinking", "done"}},
		{"claude", `{"system":"sys","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"shell","input":{"cmd":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"/home"}]}]}`, `{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"ok"}]}`, []string{"sys", "shell", "/home", "hmm", "ok"}},
		{"gemini", `{"contents":[{"role":"model","parts":[{"functionCall":{"id":"x","name":"calc","args":{"n":1}}}]},{"role":"user","parts":[{"functionResponse":{"id":"x","name":"calc","response":{"n":2}}}]}]}`, `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"reason"},{"text":"2"}]}}]}`, []string{"calc", "reason", "2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msgs := Conversation(c.req, c.res)
			b, _ := json.Marshal(msgs)
			for _, want := range c.want {
				if !strings.Contains(string(b), want) {
					t.Fatalf("missing %q: %s", want, b)
				}
			}
		})
	}
}
func TestImagesPreserveOriginalData(t *testing.T) {
	cases := []struct{ req, res, want string }{
		{`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, `{}`, "data:image/png;base64,AAAA"},
		{`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"BBBB"}}]}]}`, `{}`, "data:image/jpeg;base64,BBBB"},
		{`{"contents":[{"parts":[{"inlineData":{"mimeType":"image/webp","data":"CCCC"}}]}]}`, `{}`, "data:image/webp;base64,CCCC"},
		{`{}`, `{"data":[{"b64_json":"DDDD"}]}`, "data:image/png;base64,DDDD"},
		{`{}`, `{"output":[{"type":"image_generation_call","output_format":"jpeg","result":"EEEE"}]}`, "data:image/jpeg;base64,EEEE"},
		{`{}`, `{"candidates":[{"content":{"role":"model","parts":[{"inline_data":{"mime_type":"image/png","data":"FFFF"}}]}}]}`, "data:image/png;base64,FFFF"},
		{`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://images.example/original.png"}]}]}`, `{}`, "https://images.example/original.png"},
	}
	for i, c := range cases {
		msgs := Conversation(c.req, c.res)
		found := false
		for _, m := range msgs {
			for _, im := range m.Images {
				if im.URL == c.want {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("case %d: %+v", i, msgs)
		}
	}
}
func TestSSEToolFragmentsAndFinalSnapshot(t *testing.T) {
	raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"calc\",\"arguments\":\"{\\\"n\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"2}\"}}]}}]}\n\ndata: [DONE]\n\n"
	c := Conversation(`{}`, raw)
	if len(c) != 1 || c[0].Name != "calc" || c[0].ToolCallID != "c1" || c[0].Content != `{"n":2}` {
		t.Fatal(c)
	}
	final := "event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\r\n\r\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello world\"}]}]}}\r\n\r\n"
	c = Conversation(`{}`, final)
	if len(c) != 1 || c[0].Content != "hello world" {
		t.Fatal(c)
	}
}
func TestClaudeSSEThinkingAndTools(t *testing.T) {
	ev := []string{`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c","name":"shell","input":{}}}`, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"pwd\"}"}}`}
	raw := ""
	for _, v := range ev {
		raw += "data: " + v + "\n\n"
	}
	c := Conversation(`{}`, raw)
	if len(c) != 2 || c[0].Content != "reason" || c[1].Content != `{"cmd":"pwd"}` || c[1].Name != "shell" {
		t.Fatal(c)
	}
}
func TestPreviousResponseHistoryNotInvented(t *testing.T) {
	c := Conversation(`{"previous_response_id":"old","input":"hi"}`, `{}`)
	if len(c) != 2 || c[0].Kind != "notice" || c[1].Content != "hi" {
		t.Fatal(c)
	}
}
