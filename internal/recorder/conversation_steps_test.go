package recorder

import (
	"encoding/json"
	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestConversationStepGroupingAndToolBoundaries(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "instructions"}, {Role: "developer", Content: "instructions"},
		{Role: "user", Kind: "text", Content: "question"}, {Role: "user", Kind: "image", Images: []Image{{URL: "original"}}},
		{Role: "assistant", Kind: "reasoning", Content: "thought"}, {Role: "assistant", Kind: "text", Content: "answer"},
		{Role: "assistant", Kind: "tool_call", ToolCallID: "a", Content: "{}"}, {Role: "assistant", Kind: "tool_call", ToolCallID: "b", Content: "{}"},
		{Role: "tool", Kind: "tool_result", ToolCallID: "a", Content: "result"}, {Role: "tool", Kind: "tool_result", ToolCallID: "a", Images: []Image{{URL: "original"}}},
		{Role: "tool", Kind: "tool_result", ToolCallID: "b", Content: "result"},
		{Role: "assistant", Kind: "text", Content: "final", ResponseID: "r1"}, {Role: "assistant", Kind: "image", Images: []Image{{URL: "output"}}, ResponseID: "r1"},
		{Role: "assistant", Kind: "text", Content: "alternate", Choice: 1},
		{Role: "assistant", Kind: "text", Content: "next response", ResponseID: "r2"},
	}
	if got := conversationSteps(messages); got != 8 {
		t.Fatal("wrong step count", got)
	}
}

func TestConversationStepsBackfillCannotOverwriteANewerSnapshot(t *testing.T) {
	s := conversationStore(t)
	r := conversationTurn("first", "key", "session", `{"input":"question"}`, chatReply("answer"), 1)
	check(t, s.Save(r))
	if r.ConversationSteps == nil || *r.ConversationSteps != 2 {
		t.Fatal(r.Summary)
	}
	// Simulate an existing database whose summaries predate the new field.
	check(t, s.update(0, func(tx *bolt.Tx) error {
		var row storedRecord
		if err := json.Unmarshal(tx.Bucket(records).Get([]byte(r.ID)), &row); err != nil {
			return err
		}
		row.ConversationSteps = nil
		raw, _ := json.Marshal(row)
		summary, _ := json.Marshal(row.Summary)
		if err := tx.Bucket(records).Put([]byte(r.ID), raw); err != nil {
			return err
		}
		return tx.Bucket(summaries).Put([]byte(r.ID), summary)
	}))
	check(t, s.fillConversationSteps(r.ID, r.RequestID, 2))
	got, err := s.Get(r.ID)
	check(t, err)
	if got.ConversationSteps == nil || *got.ConversationSteps != 2 {
		t.Fatal("old snapshot not backfilled")
	}
	next := conversationTurn("second", "key", "session", `{"input":"followup"}`, chatReply("answer"), 2)
	check(t, s.Save(next))
	check(t, s.update(0, func(tx *bolt.Tx) error {
		var row storedRecord
		if err := json.Unmarshal(tx.Bucket(records).Get([]byte(next.ID)), &row); err != nil {
			return err
		}
		row.ConversationSteps = nil
		raw, _ := json.Marshal(row)
		return tx.Bucket(records).Put([]byte(next.ID), raw)
	}))
	check(t, s.fillConversationSteps(next.ID, r.RequestID, 999))
	got, err = s.Get(next.ID)
	check(t, err)
	if got.ConversationSteps != nil {
		t.Fatal("stale backfill wrote a newer snapshot")
	}
	check(t, s.fillConversationSteps(next.ID, next.RequestID, 4))
	got, err = s.Get(next.ID)
	check(t, err)
	if got.ConversationSteps == nil || *got.ConversationSteps != 4 {
		t.Fatal("old backfill overwrote newer history", got.ConversationSteps)
	}
}

func TestLatestRequestInputIncludesIndependentCacheBuckets(t *testing.T) {
	r := &Record{}
	applyUsage(r, wire.Usage{RequestID: "claude-attempt", Provider: "claude", Detail: wire.Tokens{InputTokens: 10, CacheReadTokens: 100, CacheCreationTokens: 50, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 165}})
	applyUsage(r, wire.Usage{RequestID: "openai-attempt", Provider: "openai", Detail: wire.Tokens{InputTokens: 10, CachedTokens: 8, OutputTokens: 5, ReasoningTokens: 2, TotalTokens: 15}})
	if r.RequestInputTokens == nil || *r.RequestInputTokens != 170 || r.Tokens.TotalTokens != 180 || r.Tokens.OutputTokens != 10 {
		t.Fatal("mixed protocol attempts double-counted cache or reasoning", r.Summary)
	}
}
