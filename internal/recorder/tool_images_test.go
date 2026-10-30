package recorder

import (
	"bytes"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

func TestToolImageFormatsAreExtractedWithoutBase64Text(t *testing.T) {
	source := inlineFixture(t)
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	nested := `[{"image_url":` + quote(source) + `}]`
	for name, raw := range map[string]string{
		"chat JSON string":      `{"messages":[{"role":"tool","tool_call_id":"call","name":"Read","content":` + quote(nested) + `}]}`,
		"responses JSON string": `{"input":[{"type":"function_call_output","call_id":"call","output":` + quote(nested) + `}]}`,
		"untyped image block":   `{"messages":[{"role":"tool","tool_call_id":"call","content":` + nested + `}]}`,
		"Claude source":         `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":` + quote(strings.TrimPrefix(source, "data:image/png;base64,")) + `}}]}]}]}`,
		"MCP image":             `{"input":[{"type":"function_call_output","call_id":"call","output":[{"type":"image","mimeType":"image/png","data":` + quote(strings.TrimPrefix(source, "data:image/png;base64,")) + `}]}]}`,
		"Gemini nested JSON":    `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"Read","id":"call","response":{"result":` + quote(nested) + `}}}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			messages := requestMessages(raw)
			if len(messages) != 1 || messages[0].Kind != "tool_result" || len(messages[0].Images) != 1 || messages[0].Images[0].URL != source || strings.Contains(messages[0].Content, "base64") {
				t.Fatalf("tool image not extracted: %+v", messages)
			}
			before, _ := json.Marshal(messages)
			normalizeToolImages(&messages[0])
			after, _ := json.Marshal(messages)
			if !bytes.Equal(before, after) {
				t.Fatal("normalization must be idempotent")
			}
		})
	}
}

func TestToolImageExtractionPreservesOtherJSONAndStableImageOrder(t *testing.T) {
	source := inlineFixture(t)
	raw := `{"z":{"image_url":"https://images.example/z.png"},"a":{"image_url":"` + source + `"},"n":9007199254740993,"empty":[],"null":null,"text":"OCR result","nested":"{\"empty\":[],\"null\":null}"}`
	var expected []Image
	for i := 0; i < 20; i++ {
		m := message("tool", "tool_result", raw)
		normalizeToolImages(&m)
		if len(m.Images) != 2 || strings.Contains(m.Content, "base64") || !strings.Contains(m.Content, "9007199254740993") || !strings.Contains(m.Content, "OCR result") {
			t.Fatal(m)
		}
		var remaining map[string]json.RawMessage
		check(t, json.Unmarshal([]byte(m.Content), &remaining))
		if string(remaining["empty"]) != "[]" || string(remaining["null"]) != "null" || string(remaining["nested"]) != `"{\"empty\":[],\"null\":null}"` {
			t.Fatal("changed unrelated JSON", m.Content)
		}
		if i == 0 {
			expected = m.Images
		} else if !reflect.DeepEqual(expected, m.Images) {
			t.Fatal("image order changed between parses")
		}
	}
	for _, raw := range []string{`ordinary tool text`, `{"n":9007199254740993,"items":[],"text":"image_url is only a label"}`} {
		m := message("tool", "tool_result", raw)
		normalizeToolImages(&m)
		if m.Content != raw || len(m.Images) != 0 {
			t.Fatal("changed non-image tool result")
		}
	}
}

func TestExistingToolJSONIsArchivedOnOpenAndPreviewedLocally(t *testing.T) {
	e := testEngine(t)
	source := inlineFixture(t)
	toolJSON := `[{"image_url":"` + source + `"}]`
	oldHistory, _ := json.Marshal([]Message{{Role: "tool", Kind: "tool_result", ToolCallID: "call", Name: "Read", Content: toolJSON}})
	r := &Record{Summary: Summary{ID: "old-tool-image"}, ConversationHistory: string(oldHistory), RequestBody: `{"input":[]}`}
	check(t, e.store.Save(r))
	check(t, e.store.SetImages(r.ID, []ImageAsset{}))
	check(t, e.store.FinishImages(r.ID)) // the old parser thought there were no images
	request := wire.ManagementRequest{Method: "GET", Path: APIBase + "/record", Query: url.Values{"id": {r.ID}, "view": {"chat"}}}
	response := e.Management(request, nil)
	if response.StatusCode != 200 || bytes.Contains(response.Body, []byte("base64,")) {
		t.Fatal("replay leaked inline payload", response.StatusCode)
	}
	assets := awaitAssets(t, e.store, r.ID)
	if len(assets) != 1 || assets[0].Status != "ready" {
		t.Fatal(assets)
	}
	preview := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: url.Values{"record_id": {r.ID}, "id": {assets[0].ID}, "variant": {"preview"}}}, nil)
	if preview.StatusCode != 200 || !bytes.HasPrefix(preview.Body, []byte("RIFF")) || string(preview.Body[8:12]) != "WEBP" {
		t.Fatal("no local WebP preview", preview.StatusCode)
	}
	original := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: url.Values{"record_id": {r.ID}, "id": {assets[0].ID}, "variant": {"original"}}}, nil)
	if original.StatusCode != 200 || !bytes.Equal(original.Body, imageFixture(t)) {
		t.Fatal("original image changed")
	}
	saved, err := e.Get(r.ID)
	check(t, err)
	if !strings.Contains(saved.ConversationHistory, source) {
		t.Fatal("opening replay rewrote the original stored payload")
	}
}
