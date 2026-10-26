package recorder

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

func TestMarkdownExportsCompleteStoredHistoryAndOriginalImages(t *testing.T) {
	e := testEngine(t)
	source := inlineFixture(t)
	history := []Message{
		{Role: "system", Kind: "text", Content: "系统提示"},
		{Role: "developer", Kind: "text", Content: "开发者提示"},
		{Role: "user", Kind: "text", Content: "第一轮提问", Images: []Image{{URL: source}}},
		{Role: "assistant", Kind: "reasoning", Content: "已公开的思考摘要"},
		{Role: "assistant", Kind: "tool_call", Name: "search", ToolCallID: "call-1", Content: `{"query":"中文检索"}`},
		{Role: "tool", Kind: "tool_result", Name: "search", ToolCallID: "call-1", Content: "工具返回\n```\n工具内的代码围栏"},
		{Role: "assistant", Kind: "text", Content: "第一轮回答"},
		{Role: "user", Kind: "text", Content: "最新提问", Images: []Image{{URL: source}}},
		{Role: "assistant", Kind: "text", Content: "## 最新回答\n\n保留 **Markdown** 格式。"},
	}
	data, err := json.Marshal(history)
	check(t, err)
	r := &Record{Summary: Summary{ID: "markdown", StartedAt: time.Now(), ResponseModel: "actual-model", KeyID: fingerprint("client-key"), Warnings: []string{"部分内容可能缺失"}}, ConversationHistory: string(data), RequestBody: `{"input":"最新提问"}`, ResponseBody: `{"output_text":"最新回答"}`}
	check(t, e.store.Save(r))
	check(t, e.Alias(r.KeyID, "", "测试用户"))
	// The exporter must use the persisted full history, not the latest raw request.
	response := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/export/markdown", Query: url.Values{"id": {r.ID}}}, nil)
	if response.StatusCode != 200 || response.Headers.Get("Content-Type") != "text/markdown; charset=utf-8" || response.Headers.Get("Content-Disposition") != `attachment; filename="conversation-`+r.ID+`.md"` || response.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("Markdown response: %d %v %s", response.StatusCode, response.Headers, response.Body)
	}
	text := string(response.Body)
	previous := -1
	for _, want := range []string{"系统提示", "开发者提示", "第一轮提问", "已公开的思考摘要", `{"query":"中文检索"}`, "工具返回", "第一轮回答", "最新提问", "## 最新回答"} {
		position := strings.Index(text, want)
		if position <= previous {
			t.Fatalf("missing or reordered message %q in %s", want, text)
		}
		previous = position
	}
	for _, want := range []string{"actual-model", "测试用户", "工具：search", "调用 ID：call-1", "````text\n工具返回\n```\n工具内的代码围栏\n````", "保留 **Markdown** 格式。", "采集提示：部分内容可能缺失"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Count(text, source) != 1 || strings.Count(text, "![原图][image-1]") != 2 {
		t.Fatal("original image must be embedded once with two references", text)
	}
	if strings.Contains(text, "client-key") {
		t.Fatal("caller credential added to Markdown metadata")
	}
	// An exact output budget must succeed; one byte less must reject the whole file.
	saved, err := e.Get(r.ID)
	check(t, err)
	e.cfg.MaxBodyBytes = len(response.Body)
	exact, err := e.renderMarkdown(saved)
	check(t, err)
	if string(exact) != text {
		t.Fatal("output changed at exact size limit")
	}
	e.cfg.MaxBodyBytes--
	if _, err := e.renderMarkdown(saved); !errors.Is(err, errMarkdownTooLarge) {
		t.Fatal("missing size rejection", err)
	}
	tooLarge := e.markdownResponse(r.ID)
	if tooLarge.StatusCode != 413 || !json.Valid(tooLarge.Body) {
		t.Fatal("oversized export must return an error, not a partial Markdown file")
	}
}

func TestMarkdownUsesArchivedRemoteImageWithoutRemoteLinks(t *testing.T) {
	e := testEngine(t)
	source := "https://image-tracker.invalid/private.png"
	r := recordWithImage("archived-markdown", source)
	check(t, e.store.Save(r))
	data := imageFixture(t)
	hash, err := e.images.put(data)
	check(t, err)
	check(t, e.store.SetImages(r.ID, []ImageAsset{{ID: imageID(source), SourceURL: source, Status: "ready", Hash: hash, MIME: "image/png", Size: int64(len(data))}}))
	response := e.markdownResponse(r.ID)
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data)) || strings.Contains(string(response.Body), "image-tracker.invalid") {
		t.Fatalf("archived original not exported locally: %d %s", response.StatusCode, response.Body)
	}
	// Export must surface image backpressure rather than silently omit a ready image.
	for i := 0; i < cap(e.images.serving); i++ {
		e.images.serving <- struct{}{}
	}
	busy := e.markdownResponse(r.ID)
	for i := 0; i < cap(e.images.serving); i++ {
		<-e.images.serving
	}
	if busy.StatusCode != 429 {
		t.Fatal("image backpressure not reported", busy.StatusCode)
	}
	check(t, e.store.SetImages(r.ID, nil))
	missing := e.markdownResponse(r.ID)
	if missing.StatusCode != 200 || !strings.Contains(string(missing.Body), "图片未能导出") || strings.Contains(string(missing.Body), source) {
		t.Fatal("missing remote image must be explained without an external image link")
	}
	if got := e.markdownResponse("does-not-exist"); got.StatusCode != 404 {
		t.Fatal("unknown conversation must return 404", got.StatusCode)
	}
}
