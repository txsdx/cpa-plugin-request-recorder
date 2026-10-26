package recorder

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

// CPA's Responses translator returns complete data: lines without SSE blank
// delimiters; the HTTP writer adds framing after the plugin callback.
func TestResponsesCallbackLinesPreferTerminalSnapshot(t *testing.T) {
	e := testEngine(t)
	check(t, e.Request(wire.Request{RequestID: "responses-lines", Stream: true, Body: []byte(`{"input":"question"}`)}, false))
	payloads := []string{
		`data: {"type":"response.created","response":{"id":"resp_test"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"intermediate text"}`,
		`data: {"type":"response.output_text.done","output_index":0,"text":"full text"}`,
		`data: {"type":"response.completed","response":{"id":"resp_test","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"authoritative final answer"}]}],"usage":{"output_tokens":7}}}`,
	}
	for i, payload := range payloads {
		check(t, e.Response(wire.Response{RequestID: "responses-lines", ChunkIndex: i, Body: []byte(payload)}, true))
	}
	if a := e.active["responses-lines"]; a.snapshot == "" || a.response.Len() != 0 {
		t.Fatal("full terminal snapshot did not replace intermediate events")
	}
	check(t, e.Complete(wire.Completion{RequestID: "responses-lines", Outcome: "succeeded"}))
	r, err := e.Get("responses-lines")
	check(t, err)
	var body map[string]any
	check(t, json.Unmarshal([]byte(r.ResponseBody), &body))
	if body["id"] != "resp_test" || body["object"] != "response" || r.LatestResponse != "authoritative final answer" || strings.Contains(r.ResponseBody, "intermediate text") {
		t.Fatalf("final snapshot not captured: %s", r.ResponseBody)
	}
	if !strings.Contains(r.ConversationHistory, "authoritative final answer") {
		t.Fatal("latest model response is absent from conversation")
	}
	if r.ResponseSource != "cpa_final_snapshot" {
		t.Fatal("response was reconstructed from deltas", r.ResponseSource)
	}
}

func TestFinalSnapshotFramingAndEmptyOutputFallback(t *testing.T) {
	final := `{"id":"resp_final","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final text with data: inside"}]}]}`
	initial := `data: {"type":"response.output_text.delta","output_index":0,"delta":"old text"}`
	completed := `data: {"type":"response.completed","response":` + final + `}`
	for _, separator := range []string{"", "\n", "\n\n", "\r\n\r\n"} {
		got, ok := FinalResponse(initial + separator + completed)
		if !ok || str(decode(got)["id"]) != "resp_final" || strings.Contains(got, "old text") {
			t.Fatalf("separator %q: %s", separator, got)
		}
	}
	item := `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"complete item"}]}}`
	empty := `data: {"type":"response.completed","response":{"id":"resp_empty","output":[]}}`
	got, ok := FinalResponse(initial + "\n\n" + item + "\n\n" + empty)
	if !ok || str(decode(got)["id"]) != "resp_empty" || !strings.Contains(got, "complete item") || strings.Contains(got, "old text") {
		t.Fatal("complete output item was not preferred to deltas", got)
	}
	got, ok = FinalResponse(initial + "\n\n" + empty)
	if !ok || !strings.Contains(got, "old text") || str(decode(got)["id"]) != "resp_empty" {
		t.Fatal("missing delta fallback when complete output is unavailable", got)
	}
	if _, ok = FinalResponse("data: broken event"); ok {
		t.Fatal("unrecognized stream was marked successfully parsed")
	}
}

func TestTerminalSnapshotReclaimsStreamBudget(t *testing.T) {
	e := testEngine(t)
	e.cfg.MaxMemoryBytes = 2048
	e.cfg.MaxBodyBytes = 2048
	check(t, e.Request(wire.Request{RequestID: "budget", Stream: true, Body: []byte(`{"input":"hi"}`)}, false))
	chunk := `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 2500) + `"}`
	check(t, e.Response(wire.Response{RequestID: "budget", Body: []byte(chunk)}, true))
	final := `data: {"type":"response.completed","response":{"id":"resp_budget","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"small complete answer"}]}]}}`
	check(t, e.Response(wire.Response{RequestID: "budget", Body: []byte(final)}, true))
	if e.used > 512 || e.active["budget"].response.Len() != 0 {
		t.Fatal("intermediate response retained after complete snapshot", e.used)
	}
	if len(e.active["budget"].record.Warnings) > 0 {
		t.Fatal("a complete snapshot was incorrectly marked truncated")
	}
	check(t, e.Complete(wire.Completion{RequestID: "budget", Outcome: "succeeded"}))
	if e.used != 0 {
		t.Fatal("memory not released", e.used)
	}
}
