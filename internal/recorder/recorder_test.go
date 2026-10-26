package recorder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	"github.com/txsdx/cpa-plugin-request-recorder/web"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	c := DefaultConfig()
	c.DBPath = filepath.Join(t.TempDir(), "test.db")
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	return e
}
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func TestLifecycleUsageEitherOrderAndPersistence(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			e := testEngine(t)
			now := time.Now().UTC()
			req := `{"model":"alias","messages":[{"role":"user","content":"hello"}]}`
			check(t, e.Request(wire.Request{RequestID: "r", TraceID: "abcd1234", SourceFormat: "openai", Model: "real", RequestedModel: "alias", Body: []byte(req), Headers: map[string][]string{"Authorization": {"Bearer do-not-store"}, "User-Agent": {"test"}}}, false))
			check(t, e.Response(wire.Response{RequestID: "r", OriginalRequest: []byte(req), Body: []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`), StatusCode: 200}, false))
			u := wire.Usage{RequestID: "r", APIKey: "sk-test-secret", Provider: "test", Model: "real", TTFT: 123 * time.Millisecond, Detail: wire.Tokens{InputTokens: 100, OutputTokens: 10, CachedTokens: 50, TotalTokens: 110}}
			if !late {
				check(t, e.Usage(u))
			}
			check(t, e.Complete(wire.Completion{RequestID: "r", Outcome: "succeeded", StatusCode: 200, StartedAt: now, CompletedAt: now.Add(time.Second)}))
			if late {
				check(t, e.Usage(u))
			}
			check(t, e.Alias("", "sk-test-secret", "我的电脑"))
			r, err := e.Get("r")
			check(t, err)
			if r.KeyAlias != "我的电脑" || r.KeyID != fingerprint(u.APIKey) || r.ResponseBody == "" || r.RequestBody != req || r.DurationMS != 1000 || r.TTFTMS == nil || *r.TTFTMS != 123 || r.CacheRatio == nil || *r.CacheRatio != .5 {
				t.Fatalf("unexpected record: %+v", r)
			}
			data, _ := json.Marshal(r)
			if strings.Contains(string(data), u.APIKey) || strings.Contains(string(data), "do-not-store") {
				t.Fatal("credential persisted")
			}
			page, err := e.List(Filter{Alias: "我的", Model: "real"})
			check(t, err)
			if page.Total != 1 {
				t.Fatal(page)
			}
			check(t, e.Complete(wire.Completion{RequestID: "r", Outcome: "failed"}))
			again, err := e.Get("r")
			check(t, err)
			if again.Outcome != "succeeded" {
				t.Fatal("duplicate completion overwritten")
			}
		})
	}
}
func TestStreamingBeyondHostHistoryAndCancellation(t *testing.T) {
	e := testEngine(t)
	check(t, e.Request(wire.Request{RequestID: "s", Stream: true, Body: []byte(`{"messages":[]}`)}, false))
	check(t, e.Response(wire.Response{RequestID: "s", ChunkIndex: -1, OriginalRequest: []byte(`{"messages":[]}`)}, true))
	for i := 0; i < 160; i++ {
		chunk := fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%d,\"}}]}\n\n", i)
		check(t, e.Response(wire.Response{RequestID: "s", ChunkIndex: i, Body: []byte(chunk)}, true))
	}
	check(t, e.Complete(wire.Completion{RequestID: "s", Outcome: "canceled", StatusCode: 499}))
	r, err := e.Get("s")
	check(t, err)
	if !strings.Contains(r.ResponseBody, `"0,"`) || !strings.Contains(r.ResponseBody, `"159,"`) || r.TTFTMS != nil || r.FirstChunkMS == nil || len(r.Warnings) == 0 {
		t.Fatalf("incomplete stream bookkeeping: %+v", r.Summary)
	}
	c := Conversation(r.RequestBody, r.ResponseBody)
	if len(c) != 1 || !strings.HasPrefix(c[0].Content, "0,1,") || !strings.HasSuffix(c[0].Content, "159,") {
		t.Fatal(c)
	}
}
func TestConcurrentRequestsNeverCrossContaminate(t *testing.T) {
	e := testEngine(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint(i)
			check(t, e.Request(wire.Request{RequestID: id, Body: []byte(id)}, false))
			check(t, e.Response(wire.Response{RequestID: id, Body: []byte("response-" + id)}, false))
			check(t, e.Complete(wire.Completion{RequestID: id, Outcome: "succeeded"}))
			check(t, e.Usage(wire.Usage{RequestID: id, APIKey: "key-" + id}))
		}(i)
	}
	wg.Wait()
	for i := 0; i < 40; i++ {
		id := fmt.Sprint(i)
		r, err := e.Get(id)
		check(t, err)
		if r.ResponseBody != "response-"+id || r.KeyID != fingerprint("key-"+id) {
			t.Fatal("cross-request data leak")
		}
	}
}
func TestCapacityMarksPartialAndReleasesMemory(t *testing.T) {
	e := testEngine(t)
	e.cfg.MaxBodyBytes = 1024
	e.cfg.MaxMemoryBytes = 2048
	check(t, e.Request(wire.Request{RequestID: "big", Body: []byte(strings.Repeat("x", 2000))}, false))
	check(t, e.Response(wire.Response{RequestID: "big", Body: []byte(strings.Repeat("y", 2000))}, true))
	check(t, e.Complete(wire.Completion{RequestID: "big", Outcome: "succeeded"}))
	r, err := e.Get("big")
	check(t, err)
	if len(r.RequestBody) != 1024 || len(r.ResponseBody) != 1024 || len(r.Warnings) < 2 || e.used != 0 {
		t.Fatalf("limits: %+v used %d", r.Summary, e.used)
	}
}
func TestRetentionAndPagination(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	check(t, err)
	defer s.Close()
	for i := 0; i < 8; i++ {
		check(t, s.Save(&Record{Summary: Summary{ID: fmt.Sprint(i), StartedAt: time.Now().Add(time.Duration(i-8) * time.Hour), Outcome: "succeeded", Model: "gpt"}}))
	}
	check(t, s.Prune(30, 5))
	p, err := s.List(Filter{Offset: 1, Limit: 2, Model: "gpt"})
	check(t, err)
	if p.Total != 5 || len(p.Items) != 2 || p.Items[0].ID != "6" {
		t.Fatalf("%+v", p)
	}
	path := s.db.Path()
	check(t, s.Close())
	s, err = Open(path)
	check(t, err)
	defer s.Close()
	r, err := s.Get("7")
	check(t, err)
	if r.ID != "7" {
		t.Fatal(r)
	}
}
func TestManagementResourceDoesNotExposeData(t *testing.T) {
	e := testEngine(t)
	for _, path := range []string{ResourceBase + "/records", ResourceBase + "/../records", "/wrong"} {
		resp := e.Management(wire.ManagementRequest{Method: "GET", Path: path}, web.Files)
		if resp.StatusCode != 404 {
			t.Fatalf("%s: %+v", path, resp)
		}
	}
	resp := e.Management(wire.ManagementRequest{Method: "GET", Path: ResourceBase + "/index.html"}, web.Files)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "请求记录") {
		t.Fatal(resp)
	}
	bad := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/records", Query: map[string][]string{"from": {"bad"}}}, web.Files)
	if bad.StatusCode != 400 {
		t.Fatal(bad)
	}
}

const logFixture = "=== REQUEST INFO ===\nURL: /v1/chat/completions\n\n=== HEADERS ===\nAuthorization: [MASKED]\n\n=== REQUEST BODY ===\n{\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}\n\n=== API REQUEST 1 ===\nUpstream URL: http://upstream\n\nHeaders:\n\nBody:\n{\"model\":\"real\"}\n\n=== API RESPONSE 1 ===\nTimestamp: test\nStatus: 200\n\nBody:\n{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"hello upstream\"}}]}\n\n=== RESPONSE ===\nStatus: 200\n\n{\"downstream\":true}\n"

func TestLogCorrelationAndSourceSeparation(t *testing.T) {
	e := testEngine(t)
	dir := t.TempDir()
	e.cfg.LogsDir = dir
	check(t, e.Request(wire.Request{RequestID: "l", TraceID: "abcd1234", Body: []byte("hook-request")}, false))
	check(t, e.Response(wire.Response{RequestID: "l", Body: []byte("downstream")}, false))
	check(t, e.Complete(wire.Completion{RequestID: "l", Outcome: "succeeded"}))
	path := filepath.Join(dir, "v1-chat-completions-2026-10-26T123456-abcd1234.log")
	check(t, os.WriteFile(path, []byte(logFixture), 0600))
	j := e.jobs["l"]
	for i := 0; i < 3; i++ {
		e.enrich("l", j)
	}
	r, err := e.Get("l")
	check(t, err)
	if r.UpstreamState != "log_captured" || r.ResponseBody != "downstream" || r.RequestSource != "cpa_request_log" || !strings.Contains(r.UpstreamResponses, "hello upstream") {
		t.Fatalf("%+v", r)
	}
	if got := UpstreamConversation(r); len(got) != 1 || got[0].Content != "hello upstream" {
		t.Fatal(got)
	}
	check(t, os.WriteFile(filepath.Join(dir, "other-abcd1234.log"), []byte(logFixture), 0600))
	if _, err := findLog(dir, "abcd1234", time.Now().Add(-time.Minute)); err == nil {
		t.Fatal("ambiguous logs linked")
	}
	if _, err := findLog(dir, "../../bad", time.Now()); err == nil {
		t.Fatal("unsafe trace accepted")
	}
}

func TestDistinctReporterIDsJoinByTraceWithoutDuplicateRetryBilling(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			e := testEngine(t)
			check(t, e.Request(wire.Request{RequestID: "lifecycle", TraceID: "01234567-abcd-1234-abcd-9876543210ab", Model: "real"}, false))
			if late {
				check(t, e.Complete(wire.Completion{RequestID: "lifecycle", Outcome: "succeeded"}))
			}
			u := wire.Usage{RequestID: "reporter-one", TraceID: "01234567-abcd-1234-abcd-9876543210ab", Model: "real", APIKey: "test-secret", Failed: true, Detail: wire.Tokens{InputTokens: 20, OutputTokens: 1, TotalTokens: 21}}
			check(t, e.Usage(u))
			u.RequestID = "reporter-two"
			u.Failed = false
			u.Detail = wire.Tokens{InputTokens: 100, OutputTokens: 10, TotalTokens: 110, CachedTokens: 50}
			check(t, e.Usage(u))
			check(t, e.Usage(u))
			if !late {
				check(t, e.Complete(wire.Completion{RequestID: "lifecycle", Outcome: "succeeded"}))
			}
			r, err := e.Get("lifecycle")
			check(t, err)
			if r.Tokens == nil || r.Tokens.TotalTokens != 131 || len(r.UsageAttempts) != 2 || r.KeyID != fingerprint("test-secret") {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestAmbiguousTraceUsageIsNotAssigned(t *testing.T) {
	e := testEngine(t)
	for _, id := range []string{"a", "b"} {
		check(t, e.Request(wire.Request{RequestID: id, TraceID: "same-trace", Model: "same-model"}, false))
	}
	check(t, e.Usage(wire.Usage{RequestID: "reporter", TraceID: "same-trace", Model: "same-model", APIKey: "secret"}))
	for _, a := range e.active {
		if a.record.KeyID != "" {
			t.Fatal("ambiguous identity assigned")
		}
	}
	if len(e.pendingUsage) != 1 {
		t.Fatal("missing pending usage")
	}
}
func TestFullTraceUsesTrailingEightCharacters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v1-chat-completions-2026-10-26T123456-6543210a.log")
	check(t, os.WriteFile(path, []byte(logFixture), 0600))
	found, err := findLog(dir, "01234567-abcd-1234-abcd-98766543210a", time.Now().Add(-time.Minute))
	check(t, err)
	if found != path {
		t.Fatal(found)
	}
}
func TestFailedAndRejectedRequestsHaveTerminalRecords(t *testing.T) {
	for _, outcome := range []string{"failed", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			e := testEngine(t)
			check(t, e.Request(wire.Request{RequestID: "r", Body: []byte(`{"messages":[]}`)}, false))
			check(t, e.Complete(wire.Completion{RequestID: "r", Outcome: outcome, StatusCode: 403, Error: "blocked"}))
			r, err := e.Get("r")
			check(t, err)
			if r.Outcome != outcome || r.Status != 403 || r.Error != "blocked" || r.ResponseBody != "" {
				t.Fatal(r)
			}
		})
	}
}
func TestPendingLogJobsResumeAfterRestart(t *testing.T) {
	c := DefaultConfig()
	c.DBPath = filepath.Join(t.TempDir(), "test.db")
	c.LogsDir = t.TempDir()
	e, err := New(c)
	check(t, err)
	check(t, e.Request(wire.Request{RequestID: "r", TraceID: "abcd1234"}, false))
	check(t, e.Complete(wire.Completion{RequestID: "r", Outcome: "succeeded"}))
	check(t, e.Close())
	e, err = New(c)
	check(t, err)
	defer e.Close()
	if len(e.jobs) != 1 {
		t.Fatal("pending job not restored")
	}
}
func TestOverloadDoesNotCaptureOnlyStreamSuffixAsComplete(t *testing.T) {
	e := testEngine(t)
	e.cfg.MaxInflight = 1
	check(t, e.Request(wire.Request{RequestID: "first"}, false))
	check(t, e.Request(wire.Request{RequestID: "dropped"}, false))
	check(t, e.Complete(wire.Completion{RequestID: "first", Outcome: "succeeded"}))
	for i := 0; i < 20; i++ {
		check(t, e.Response(wire.Response{RequestID: "dropped", Body: []byte("suffix")}, true))
	}
	check(t, e.Complete(wire.Completion{RequestID: "dropped", Outcome: "succeeded"}))
	if _, err := e.Get("dropped"); err != ErrNotFound {
		t.Fatal("partial suffix incorrectly captured")
	}
	if e.dropped != 1 {
		t.Fatalf("dropped request count = %d", e.dropped)
	}
}
