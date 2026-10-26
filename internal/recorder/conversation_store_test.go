package recorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func conversationStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "conversations.db"))
	check(t, err)
	t.Cleanup(func() { check(t, s.Close()) })
	return s
}
func conversationTurn(id, key, session, request, response string, turn int) *Record {
	now := time.Date(2026, 10, 26, 0, 0, turn, 0, time.UTC)
	r := &Record{Summary: Summary{ID: id, KeyID: fingerprint(key), StartedAt: now, CompletedAt: now.Add(time.Second)}, RequestBody: request, ResponseBody: response, RequestHeaders: http.Header{}}
	if session != "" {
		r.RequestHeaders.Set("X-Session-ID", session)
	}
	updatePreviews(r)
	return r
}
func chatRequest(messages ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"messages": messages})
	return string(b)
}
func chat(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}
func chatReply(content string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": chat("assistant", content)}}})
	return string(b)
}
func assertConversationCount(t *testing.T, s *Store, n int) {
	t.Helper()
	p, err := s.List(Filter{})
	check(t, err)
	if p.Total != n {
		t.Fatalf("got %d conversations, want %d: %+v", p.Total, n, p.Items)
	}
}

func TestConversationReplacesSnapshotsAndPreservesReasoningToolsImages(t *testing.T) {
	s := conversationStore(t)
	first := conversationTurn("r1", "key", "", chatRequest(chat("user", "hello")), `{"choices":[{"message":{"role":"assistant","reasoning_content":"original thought","content":"answer"}}]}`, 1)
	check(t, s.Save(first))
	stable := first.ID
	if stable == "r1" || first.ConversationHistory == "" {
		t.Fatal("missing stable identity/history")
	}
	second := conversationTurn("r2", "key", "", chatRequest(chat("user", "hello"), chat("assistant", "answer"), chat("user", "read file")), `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]}}]}`, 2)
	check(t, s.Save(second))
	assertConversationCount(t, s, 1)
	if second.ID != stable || second.ConversationTurns != 2 {
		t.Fatalf("not replaced: %+v", second.Summary)
	}
	tool := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "function": map[string]any{"name": "read", "arguments": "{ \"path\" : \"a\" }"}}}}
	result := map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "file content"}
	third := conversationTurn("r3", "key", "", chatRequest(chat("user", "hello"), chat("assistant", "answer"), chat("user", "read file"), tool, result), `{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"done"},{"type":"image_url","image_url":{"url":"data:image/png;base64,original"}}]}}]}`, 3)
	check(t, s.Save(third))
	assertConversationCount(t, s, 1)
	got, err := s.Get(stable)
	check(t, err)
	if got.RequestID != "r3" || got.ConversationTurns != 3 || !strings.Contains(got.ConversationHistory, "original thought") || !strings.Contains(got.ConversationHistory, "data:image/png;base64,original") {
		t.Fatalf("lost history: %s", got.ConversationHistory)
	}
	messages := conversationMessages(got)
	found := false
	for _, m := range messages {
		if m.Kind == "tool_result" && m.Content == "file content" && m.Name == "read" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing named tool result", messages)
	}
	if _, err := s.Get("r1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("retired request still resolves", err)
	}
	before := got.ConversationHistory
	check(t, s.Save(got))
	again, err := s.Get(stable)
	check(t, err)
	if again.ConversationHistory != before || again.ConversationTurns != 3 {
		t.Fatal("metadata save appended duplicate history")
	}
	check(t, s.Prune(0, 0))
	check(t, s.view(func(tx *bolt.Tx) error {
		if tx.Bucket(records).Stats().KeyN != 1 || tx.Bucket(requestRecords).Stats().KeyN != 1 {
			t.Fatal("obsolete records or request indices retained")
		}
		return nil
	}))
}

func TestConversationExplicitIDsSeparateCallersAndAppendIncrementalInput(t *testing.T) {
	s := conversationStore(t)
	for i, values := range [][2]string{{"a", "thread"}, {"a", "other-thread"}, {"b", "thread"}} {
		r := conversationTurn(fmt.Sprint(i), values[0], values[1], chatRequest(chat("user", "same prompt")), chatReply("first"), i+1)
		check(t, s.Save(r))
	}
	next := conversationTurn("next", "a", "thread", chatRequest(chat("user", "incremental question")), chatReply("second"), 4)
	check(t, s.Save(next))
	assertConversationCount(t, s, 3)
	if !strings.Contains(next.ConversationHistory, "same prompt") || !strings.Contains(next.ConversationHistory, "incremental question") {
		t.Fatal(next.ConversationHistory)
	}
}

func TestConversationRegenerationAndLateCompletionKeepNewestBranch(t *testing.T) {
	s := conversationStore(t)
	root := chatRequest(chat("user", "root"))
	first := conversationTurn("first", "k", "", root, chatReply("old answer"), 1)
	check(t, s.Save(first))
	stable := first.ID
	second := conversationTurn("second", "k", "", chatRequest(chat("user", "root"), chat("assistant", "old answer"), chat("user", "followup")), chatReply("old continuation"), 2)
	check(t, s.Save(second))
	regen := conversationTurn("regen", "k", "", root, chatReply("new answer"), 4)
	check(t, s.Save(regen))
	assertConversationCount(t, s, 1)
	if regen.ID != stable || strings.Contains(regen.ConversationHistory, "old answer") || strings.Contains(regen.ConversationHistory, "old continuation") {
		t.Fatal("regeneration retained stale branch", regen.ConversationHistory)
	}
	late := conversationTurn("late", "k", "", root, chatReply("late old answer"), 3)
	check(t, s.Save(late))
	got, err := s.Get(stable)
	check(t, err)
	if got.RequestID != "regen" || got.LatestResponse != "new answer" {
		t.Fatal("late completion overwrote latest", got.Summary)
	}
}

func TestConversationObservedResponsesChainAndBranch(t *testing.T) {
	s := conversationStore(t)
	first := conversationTurn("r1", "k", "", `{"input":"hello"}`, `{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"one"}]}]}`, 1)
	check(t, s.Save(first))
	second := conversationTurn("r2", "k", "", `{"previous_response_id":"resp_1","input":"followup"}`, `{"id":"resp_2","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"two"}]}]}`, 2)
	check(t, s.Save(second))
	assertConversationCount(t, s, 1)
	if !strings.Contains(second.ConversationHistory, "hello") || !strings.Contains(second.ConversationHistory, "followup") || strings.Contains(second.ConversationHistory, "不能凭空") {
		t.Fatal(second.ConversationHistory)
	}
	branch := conversationTurn("r3", "k", "", `{"previous_response_id":"resp_1","input":"different followup"}`, `{"id":"resp_3","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"three"}]}]}`, 3)
	check(t, s.Save(branch))
	assertConversationCount(t, s, 1)
	if strings.Contains(branch.ConversationHistory, `"two"`) || !strings.Contains(branch.ConversationHistory, "one") {
		t.Fatal("branch did not trim old response", branch.ConversationHistory)
	}
	missing := conversationTurn("r4", "k", "", `{"previous_response_id":"unobserved","input":"unknown"}`, chatReply("answer"), 4)
	check(t, s.Save(missing))
	assertConversationCount(t, s, 2)
	if !strings.Contains(missing.ConversationHistory, "不能凭空") {
		t.Fatal("unobserved history fabricated")
	}
}

func TestConversationScopeAndConfigurationAvoidFalseMerges(t *testing.T) {
	s := conversationStore(t)
	for i, body := range []string{chatRequest(chat("user", "one")), chatRequest(chat("user", "different")), chatRequest(chat("system", "distinct instructions"), chat("user", "one"))} {
		r := conversationTurn(fmt.Sprint(i), "same-key", "", body, chatReply("answer"), i+1)
		r.SessionID = "host-routing-id"
		check(t, s.Save(r))
	}
	assertConversationCount(t, s, 3)
}

func TestConversationLateKeyCoalescesOrphanAndHistoryLimitPreservesSnapshot(t *testing.T) {
	s := conversationStore(t)
	first := conversationTurn("first", "k", "thread", chatRequest(chat("user", "first")), chatReply("one"), 1)
	check(t, s.Save(first))
	orphan := conversationTurn("orphan", "k", "thread", chatRequest(chat("user", "second")), chatReply("two"), 2)
	orphan.KeyID = ""
	check(t, s.Save(orphan))
	assertConversationCount(t, s, 2)
	orphan.KeyID = fingerprint("k")
	check(t, s.Save(orphan))
	assertConversationCount(t, s, 1)
	if orphan.ID != first.ID || !strings.Contains(orphan.ConversationHistory, "first") || !strings.Contains(orphan.ConversationHistory, "second") {
		t.Fatal("late key lost history", orphan.ConversationHistory)
	}
	s.historyLimit = len(orphan.ConversationHistory) + 100
	oversize := conversationTurn("large", "k", "thread", chatRequest(chat("user", strings.Repeat("z", 4096))), chatReply("three"), 3)
	if err := s.Save(oversize); !errors.Is(err, ErrConversationTooLarge) {
		t.Fatal(err)
	}
	got, err := s.Get(first.ID)
	check(t, err)
	if got.RequestID != "orphan" {
		t.Fatal("oversize erased complete snapshot")
	}
	check(t, s.Prune(0, 1))
	got.StartedAt = time.Now().AddDate(0, 0, -10)
	check(t, s.Save(got))
	check(t, s.Prune(1, 0))
	check(t, s.view(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{records, conversationKeys, conversationAliases, requestRecords, bodyCounts, bodyChunks} {
			if tx.Bucket(bucket).Stats().KeyN != 0 {
				t.Fatalf("prune leaked %s", bucket)
			}
		}
		return nil
	}))
}
