package recorder

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

const cherrySessionA = "cherry-agent:0123456789abcdef0123456789abcdef"
const cherrySessionB = "cherry-agent:fedcba9876543210fedcba9876543210"

func TestClientConversationIdentitySources(t *testing.T) {
	for _, tc := range []struct {
		name, body, host, want string
		headers                http.Header
	}{
		{name: "codex hyphen", headers: http.Header{"Session-Id": {"thread"}}, want: "client:thread"},
		{name: "codex underscore lowercase", headers: http.Header{"session_id": {"thread"}}, want: "client:thread"},
		{name: "codex turn metadata", headers: http.Header{"X-Codex-Turn-Metadata": {`{"session_id":"parent","thread_id":"child"}`}}, want: "client:child"},
		{name: "codex named child", headers: http.Header{"Session-Id": {"parent"}, "X-Codex-Turn-Metadata": {`{"agent_name":"/root/research","subagent_kind":"thread_spawn"}`}}, want: "client:parent:agent:research"},
		{name: "codex unidentified child", headers: http.Header{"Session-Id": {"parent"}, "X-Openai-Subagent": {"true"}}},
		{name: "codex reused parent thread", headers: http.Header{"Session-Id": {"parent"}, "Thread-Id": {"parent"}, "X-Openai-Subagent": {"true"}}},
		{name: "generic header lowercase", headers: http.Header{"x-conversation-id": {"thread"}}, want: "client:thread"},
		{name: "thread underscore", headers: http.Header{"Thread_id": {"thread"}}, want: "client:thread"},
		{name: "nested camel case", body: `{"request":{"metadata":{"threadId":"thread"}}}`, want: "client:thread"},
		{name: "conversation string", body: `{"conversation":"thread"}`, want: "client:thread"},
		{name: "extra body", body: `{"extra_body":{"session_id":"thread"}}`, want: "client:thread"},
		{name: "cherry cache key", body: `{"prompt_cache_key":"` + cherrySessionA + `"}`, want: "client:pck:" + cherrySessionA},
		{name: "cherry camel key", body: `{"promptCacheKey":"` + cherrySessionA + `"}`, want: "client:pck:" + cherrySessionA},
		{name: "explicit beats cache", body: `{"session_id":"thread","prompt_cache_key":"` + cherrySessionA + `"}`, want: "client:thread"},
		{name: "claude JSON", body: `{"metadata":{"user_id":"{\"session_id\":\"thread\",\"agent_id\":\"reader\"}"}}`, want: "client:claude:thread:agent:reader"},
		{name: "claude legacy", body: `{"metadata":{"user_id":"user_user_account_account_session_11111111-2222-4333-8444-555555555555"}}`, want: "client:claude:11111111-2222-4333-8444-555555555555"},
		{name: "claude child header", headers: http.Header{"X-Claude-Code-Session-Id": {"thread"}, "X-Claude-Code-Agent-Id": {"reader"}}, want: "client:claude:thread:agent:reader"},
		{name: "host codex", host: "codex:thread", want: "client:thread"},
		{name: "host codex child", host: "codex:thread:agent:reader", want: "client:thread:agent:reader"},
		{name: "host claude child", host: "claude:thread:agent:reader", want: "client:claude:thread:agent:reader"},
		{name: "host cherry", host: "pck:" + cherrySessionA, want: "client:pck:" + cherrySessionA},
		{name: "shared cache", body: `{"prompt_cache_key":"shared-cache"}`, host: "pck:shared-cache"},
		{name: "incomplete cherry", body: `{"prompt_cache_key":"cherry-agent:short"}`, host: "pck:cherry-agent:short"},
		{name: "plain user", body: `{"metadata":{"user_id":"shared-user"}}`, host: "user:shared-user"},
		{name: "execution", host: "execution:run"},
		{name: "derived context", host: "derived:ctx:v1:root"},
		{name: "canonical opaque", host: "11111111-2222-4333-8444-555555555555"},
		{name: "request identity", headers: http.Header{"X-Client-Request-Id": {"per-request"}}, host: "clientreq:per-request"},
		{name: "control body", body: `{"session_id":"thread\n"}`},
		{name: "control header", headers: http.Header{"X-Session-Id": {"thread\x00"}}},
		{name: "oversized", body: `{"session_id":"` + strings.Repeat("x", 257) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Record{Summary: Summary{SessionID: tc.host}, RequestHeaders: tc.headers, RequestBody: tc.body}
			if got := clientConversationID(r); got != tc.want {
				t.Fatalf("identity %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCherrySessionPreservesTrimmedToolHistoryAndDeveloperOrder(t *testing.T) {
	s := conversationStore(t)
	request := func(input string) string {
		return `{"prompt_cache_key":"` + cherrySessionA + `","input":` + input + `}`
	}
	first := conversationTurn("r1", "key", "", request(`[
		{"role":"developer","content":"global instruction"},
		{"role":"user","content":[{"type":"input_text","text":"inspect file"},{"type":"input_image","image_url":"data:image/png;base64,original"}]},
		{"role":"developer","content":"before tool"},
		{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_a","output":"file content"},
		{"role":"developer","content":"after tool"}
	]`), `{"output":[{"type":"reasoning","summary":[{"text":"saved thought"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}]}`, 1)
	check(t, s.Save(first))
	second := conversationTurn("r2", "key", "", request(`[
		{"type":"function_call_output","call_id":"call_a","output":"file content"},
		{"role":"developer","content":"after tool"},
		{"role":"assistant","content":"first answer"},
		{"role":"user","content":"follow up"}
	]`), chatReply("second answer"), 2)
	check(t, s.Save(second))
	assertConversationCount(t, s, 1)
	if second.ID != first.ID || second.ConversationTurns != 2 || second.ClientSessionID != "client:pck:"+cherrySessionA {
		t.Fatal(second.Summary)
	}
	history := conversationMessages(second)
	var content []string
	for _, m := range history {
		if m.Kind != "image" {
			content = append(content, m.Content)
		}
	}
	want := []string{"global instruction", "inspect file", "before tool", "{}", "file content", "after tool", "saved thought", "first answer", "follow up", "second answer"}
	if fmt.Sprint(content) != fmt.Sprint(want) || !strings.Contains(second.ConversationHistory, "data:image/png;base64,original") {
		t.Fatalf("lost/reordered transcript: %v", content)
	}
	regen := conversationTurn("r3", "key", "", request(`[{"role":"assistant","content":"first answer"},{"role":"user","content":"different follow up"}]`), chatReply("new branch"), 3)
	check(t, s.Save(regen))
	if strings.Contains(regen.ConversationHistory, "second answer") || strings.Contains(regen.ConversationHistory, `"follow up"`) || !strings.Contains(regen.ConversationHistory, "saved thought") {
		t.Fatal("trimmed regeneration retained stale branch or lost thought", regen.ConversationHistory)
	}
}

func TestSessionIdentityIsolationAndContextUpgrade(t *testing.T) {
	s := conversationStore(t)
	first := conversationTurn("first", "key", "", chatRequest(chat("user", "hello")), chatReply("one"), 1)
	check(t, s.Save(first))
	second := conversationTurn("second", "key", "", chatRequest(chat("user", "hello"), chat("assistant", "one"), chat("user", "next")), chatReply("two"), 2)
	second.SessionID = "pck:" + cherrySessionA
	check(t, s.Save(second))
	assertConversationCount(t, s, 1)
	if second.ID != first.ID || !strings.Contains(second.ConversationHistory, "hello") {
		t.Fatal("a newly available session identity split a known context")
	}
	for i, values := range [][2]string{{"key", "pck:" + cherrySessionB}, {"other-key", "pck:" + cherrySessionA}} {
		r := conversationTurn(fmt.Sprint(i), values[0], "", second.RequestBody, chatReply("independent"), i+3)
		r.SessionID = values[1]
		check(t, s.Save(r))
	}
	assertConversationCount(t, s, 3)
	// Even an identical observed response ID cannot join two explicit sessions.
	a := conversationTurn("a", "key", "session-a", `{"input":"hi"}`, `{"id":"resp_a","output":[]}`, 6)
	check(t, s.Save(a))
	b := conversationTurn("b", "key", "session-b", `{"previous_response_id":"resp_a","input":"hi"}`, chatReply("separate"), 7)
	check(t, s.Save(b))
	assertConversationCount(t, s, 5)
}

func TestBillingSessionIDsRemainUsefulAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recorder.db")
	s, err := Open(path)
	check(t, err)
	first := conversationTurn("first", "key", "", chatRequest(chat("user", "first question")), chatReply("first answer"), 1)
	first.SessionID = "codex:thread"
	check(t, s.Save(first))
	check(t, s.Close())
	s, err = Open(path)
	check(t, err)
	defer s.Close()
	second := conversationTurn("second", "key", "", chatRequest(chat("user", "new question")), chatReply("new answer"), 2)
	second.SessionID = "codex:thread"
	second.Model = "different-model"
	check(t, s.Save(second))
	assertConversationCount(t, s, 1)
	if second.ID != first.ID || !strings.Contains(second.ConversationHistory, "first answer") {
		t.Fatal("host-reported session was not used after restart")
	}
	for i, id := range []string{"codex:thread:agent:one", "codex:thread:agent:two", "claude:thread:agent:one"} {
		r := conversationTurn(id, "key", "", chatRequest(chat("user", "new question")), chatReply("child answer"), i+3)
		r.SessionID = id
		r.ParentSessionID = "codex:thread"
		check(t, s.Save(r))
	}
	assertConversationCount(t, s, 4)
}

func TestSharedPromptCacheKeysDoNotCombineUnrelatedChats(t *testing.T) {
	s := conversationStore(t)
	for i, prompt := range []string{"first unrelated question", "second unrelated question"} {
		r := conversationTurn(fmt.Sprint(i), "key", "", `{"prompt_cache_key":"shared","messages":[{"role":"user","content":"`+prompt+`"}]}`, chatReply("answer"), i+1)
		r.SessionID = "pck:shared"
		check(t, s.Save(r))
	}
	assertConversationCount(t, s, 2)
}

func TestSessionFiltersAcceptBillingAndClientIdentities(t *testing.T) {
	s := conversationStore(t)
	r := conversationTurn("first", "key", "", `{"prompt_cache_key":"`+cherrySessionA+`","input":"hello"}`, chatReply("answer"), 1)
	// A canonical metadata UUID differs from Billing's protocol-prefixed ID.
	r.SessionID = "11111111-2222-4333-8444-555555555555"
	check(t, s.Save(r))
	for _, query := range []string{r.SessionID, cherrySessionA, "pck:" + cherrySessionA, r.ClientSessionID} {
		page, err := s.List(Filter{Session: query})
		check(t, err)
		if page.Total != 1 {
			t.Fatal("session filter did not find client identity", query)
		}
	}
	page, err := s.List(Filter{Query: cherrySessionA})
	check(t, err)
	if page.Total != 1 {
		t.Fatal("client identity missing from text search")
	}
}

func TestRepeatedIncrementalUserPromptDoesNotTrimConversation(t *testing.T) {
	s := conversationStore(t)
	for i, prompt := range []string{"continue", "continue", "continue"} {
		r := conversationTurn(fmt.Sprint(i), "key", "thread", chatRequest(chat("user", prompt)), chatReply(fmt.Sprint("answer ", i)), i+1)
		check(t, s.Save(r))
		if r.ConversationTurns != i+1 || len(conversationMessages(r)) != (i+1)*2 {
			t.Fatal(r.ConversationHistory)
		}
	}
}
