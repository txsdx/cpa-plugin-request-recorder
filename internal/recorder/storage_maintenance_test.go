package recorder

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapacityEvictsOldestAndShrinksActiveDatabase(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "capacity.db"))
	check(t, err)
	defer s.Close()
	s.limit = 8 << 20
	random := rand.New(rand.NewSource(42))
	for i := 0; i < 30; i++ {
		body := make([]byte, 384<<10)
		_, err := random.Read(body)
		check(t, err)
		check(t, s.Save(&Record{Summary: Summary{ID: fmt.Sprint(i), StartedAt: time.Unix(int64(i), 0)}, ResponseBody: string(body)}))
	}
	before := s.databaseSize()
	if before.File <= s.limit {
		t.Fatal("fixture did not exceed cap", before)
	}
	s.requestMaintenance()
	s.wg.Wait()
	if s.lastMaintenanceError != "" {
		t.Fatal(s.lastMaintenanceError)
	}
	after := s.databaseSize()
	if after.File > s.limit || after.Evicted == 0 || after.Records == 0 {
		t.Fatal("capacity not enforced", before, after)
	}
	if _, err := s.Get("0"); err != ErrNotFound {
		t.Fatal("oldest record remained")
	}
	if _, err := s.Get("29"); err != nil {
		t.Fatal("latest record lost", err)
	}
	check(t, s.SetRefreshInterval(2))
	check(t, s.compactOnline())
	if s.RefreshInterval(1) != 2 {
		t.Fatal("compaction lost settings")
	}
	t.Logf("Capacity fixture: active DB %d -> %d bytes, retained=%d, evicted=%d", before.File, after.File, after.Records, after.Evicted)
}

func TestOnlineCompactionReplaysConcurrentWrites(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "online.db"))
	check(t, err)
	defer s.Close()
	random := rand.New(rand.NewSource(71))
	for i := 0; i < 180; i++ {
		body := make([]byte, 96<<10)
		_, err := random.Read(body)
		check(t, err)
		check(t, s.Save(&Record{Summary: Summary{ID: fmt.Sprint(i), StartedAt: time.Unix(int64(i), 0)}, ResponseBody: string(body)}))
	}
	compacted := make(chan error, 1)
	go func() { compacted <- s.compactOnline() }()
	active := false
	for i := 0; i < 1000; i++ {
		s.writes.Lock()
		active = s.journal != nil
		s.writes.Unlock()
		if active {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !active {
		t.Fatal("did not observe active snapshot")
	}
	for i := 0; i < 30; i++ {
		r, err := s.Get(fmt.Sprint(i))
		check(t, err)
		r.Model = fmt.Sprintf("concurrent-model-%d", i)
		check(t, s.Save(r))
		r.Model = "caller mutation after Save must not affect replay"
		check(t, s.SetAlias(fingerprint(fmt.Sprint(i)), fmt.Sprint("备注", i)))
	}
	check(t, <-compacted)
	for i := 0; i < 30; i++ {
		r, err := s.Get(fmt.Sprint(i))
		check(t, err)
		if r.Model != fmt.Sprintf("concurrent-model-%d", i) {
			t.Fatal("concurrent write lost or caller mutation replayed", r.Model)
		}
	}
	check(t, s.Prune(0, 30))
	check(t, s.compactOnline())
	page, err := s.List(Filter{})
	check(t, err)
	if page.Total != 30 {
		t.Fatal("pruning/compaction lost index consistency", page.Total)
	}
}

func TestFinalResponsesDropEventsAndRetainReasoningToolsAndImages(t *testing.T) {
	response := `{"id":"final","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"思考摘要"}]},{"type":"function_call","name":"shell","call_id":"call-1","arguments":"{\"cmd\":\"pwd\"}"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"最终答案"},{"type":"output_image","image_url":"https://example.com/final.png"}]}]}`
	raw := `data: {"type":"response.output_text.delta","delta":"不应重复的中间片段"}` + "\n\n" + `data: {"type":"response.completed","response":` + response + "}\n\n"
	final, ok := FinalResponse(raw)
	if !ok || strings.Contains(final, "data:") || strings.Contains(final, "中间片段") || !strings.Contains(final, "最终答案") || !strings.Contains(final, "shell") || !strings.Contains(final, "思考摘要") {
		t.Fatal(final)
	}
	claude := `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"保留思考"}}` + "\n\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-2","name":"calc","input":{}}}` + "\n\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}` + "\n\n"
	final, ok = FinalResponse(claude)
	msgs := responseMessages(final)
	if !ok || strings.Contains(final, "data:") || len(msgs) != 2 || msgs[0].Content != "保留思考" || msgs[1].Name != "calc" || msgs[1].Content != `{"x":1}` {
		t.Fatal(final, msgs)
	}
}

func TestFinalUpstreamAttemptAndLatestPreviews(t *testing.T) {
	raw := strings.Replace(logFixture, "=== RESPONSE ===", `=== API REQUEST 2 ===
Body:
{"model":"new","messages":[{"role":"user","content":"最新提示词"}]}
=== API RESPONSE 2 ===
Body:
data: {"choices":[{"index":0,"delta":{"content":"上游最终"}}]}

data: {"choices":[{"index":0,"delta":{"content":"答案"}}]}

=== RESPONSE ===`, 1)
	parts, err := ParseLog(raw)
	check(t, err)
	if parts.Attempt != 2 || strings.Contains(parts.UpstreamResponses, "hello upstream") || strings.Contains(parts.UpstreamResponses, "data:") || !strings.Contains(parts.UpstreamResponses, "上游最终答案") {
		t.Fatal(parts)
	}
	r := &Record{RequestBody: `{"messages":[{"role":"user","content":"旧问题"},{"role":"assistant","content":"旧答案"},{"role":"user","content":"最新问题"}]}`, UpstreamResponses: parts.UpstreamResponses}
	updatePreviews(r)
	if r.LatestPrompt != "最新问题" || r.LatestResponse != "上游最终答案" {
		t.Fatal(r.Summary)
	}
}
