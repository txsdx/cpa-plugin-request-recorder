package recorder

import (
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

func TestConversationTotalsAccumulateRequestsAndReplaceMetadataUpdates(t *testing.T) {
	s := conversationStore(t)
	first := conversationTurn("totals-first", "k", "totals", chatRequest(chat("user", "first")), chatReply("one"), 1)
	first.DurationMS = 1200
	first.Tokens = &wire.Tokens{TotalTokens: 100}
	check(t, s.Save(first))
	second := conversationTurn("totals-second", "k", "totals", chatRequest(chat("user", "next")), chatReply("two"), 2)
	second.StartedAt = first.CompletedAt.Add(time.Hour) // user idle time is not model time
	second.CompletedAt = second.StartedAt.Add(2 * time.Second)
	second.DurationMS = 2000
	check(t, s.Save(second))
	assertTotals := func(duration float64, tokens int64, tokenRequests int) {
		t.Helper()
		r, err := s.Get(first.ID)
		check(t, err)
		got := r.ConversationTotals
		if got == nil || got.DurationMS != duration || got.TotalTokens != tokens || got.DurationRequests != 2 || got.TokenRequests != tokenRequests || r.ConversationTurns != 2 {
			t.Fatalf("unexpected cumulative totals: %+v, turns=%d", got, r.ConversationTurns)
		}
	}
	assertTotals(3200, 100, 1)
	applyUsage(second, wire.Usage{RequestID: "reporter-a", Detail: wire.Tokens{TotalTokens: 50}})
	check(t, s.Save(second))
	assertTotals(3200, 150, 2)
	applyUsage(second, wire.Usage{RequestID: "reporter-a", Detail: wire.Tokens{TotalTokens: 60}})
	check(t, s.Save(second))
	assertTotals(3200, 160, 2)
	applyUsage(second, wire.Usage{RequestID: "reporter-b", Failed: true, Detail: wire.Tokens{TotalTokens: 10}})
	check(t, s.Save(second))
	assertTotals(3200, 170, 2)
	check(t, s.Save(second))
	check(t, s.compactOnline())
	assertTotals(3200, 170, 2)
	other := conversationTurn("other-user", "other-key", "totals", chatRequest(chat("user", "independent")), chatReply("independent answer"), 4)
	check(t, s.Save(other))
	assertTotals(3200, 170, 2)
}

func TestConversationTotalsCountRegenerationAndIgnoreObsoleteCompletion(t *testing.T) {
	s := conversationStore(t)
	for i, id := range []string{"initial", "regenerated"} {
		r := conversationTurn(id, "k", "thread", chatRequest(chat("user", "question")), chatReply(id), i+2)
		r.Tokens = &wire.Tokens{TotalTokens: int64((i + 1) * 10)}
		check(t, s.Save(r))
	}
	late := conversationTurn("late", "k", "thread", chatRequest(chat("user", "question")), chatReply("obsolete"), 1)
	late.Tokens = &wire.Tokens{TotalTokens: 500}
	check(t, s.Save(late))
	if late.RequestID != "regenerated" || late.ConversationTotals.TotalTokens != 30 || late.ConversationTotals.DurationMS != 2000 || late.ConversationTotals.TokenRequests != 2 {
		t.Fatalf("obsolete completion changed latest snapshot totals: %+v", late.Summary)
	}
}

func TestConversationTotalsMissingHistoricalCountersRemainPartial(t *testing.T) {
	r := conversationTurn("current", "k", "s", "", "", 1)
	r.ConversationTurns = 4
	r.Tokens = &wire.Tokens{TotalTokens: 30}
	got := conversationTotals(r)
	if got.TotalTokens != 30 || got.TokenRequests != 1 || got.DurationRequests != 1 {
		t.Fatal("missing history was fabricated", got)
	}
	updated := *r
	updated.Tokens = &wire.Tokens{TotalTokens: 40}
	updateConversationTotals(&updated, r)
	if updated.ConversationTotals.TotalTokens != 40 || updated.ConversationTotals.TokenRequests != 1 {
		t.Fatal("same request was double-counted", updated.ConversationTotals)
	}
}
