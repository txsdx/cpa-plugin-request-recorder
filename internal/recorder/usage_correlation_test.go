package recorder

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

func recordedUsageTurn(t *testing.T, e *Engine, id, trace string, start time.Time) {
	t.Helper()
	check(t, e.Request(wire.Request{RequestID: id, TraceID: trace, CallerScope: "test-caller", Model: "model", Headers: map[string][]string{"X-Session-Id": {"thread"}}, Body: []byte(chatRequest(chat("user", id)))}, false))
	check(t, e.Response(wire.Response{RequestID: id, Body: []byte(chatReply("answer-" + id))}, false))
	check(t, e.Complete(wire.Completion{RequestID: id, Outcome: "succeeded", StartedAt: start, CompletedAt: start.Add(time.Second)}))
}

func TestDelayedUsageUpdatesOldTurnTotalsWithoutReplacingCurrentRequest(t *testing.T) {
	e := testEngine(t)
	start := time.Now().Add(-time.Minute)
	recordedUsageTurn(t, e, "old", "trace-old", start)
	first, err := e.Get("old")
	check(t, err)
	recordedUsageTurn(t, e, "new", "trace-new", start.Add(2*time.Second))
	newUsage := wire.Usage{RequestID: "reporter-new", TraceID: "trace-new", APIKey: "client", Model: "model", RequestedAt: start.Add(2100 * time.Millisecond), Detail: wire.Tokens{TotalTokens: 100}}
	check(t, e.Usage(newUsage))
	before, err := e.Get(first.ID)
	check(t, err)
	storageBefore, err := e.store.StorageStats()
	check(t, err)
	oldUsage := wire.Usage{RequestID: "reporter-old", TraceID: "trace-old", APIKey: "client", Model: "model", RequestedAt: start.Add(100 * time.Millisecond), Detail: wire.Tokens{TotalTokens: 50}}
	check(t, e.Usage(oldUsage))
	check(t, e.Usage(oldUsage))
	oldUsage.Detail.TotalTokens = 60
	check(t, e.Usage(oldUsage))
	oldUsage.RequestID = "reporter-old-retry"
	oldUsage.Detail.TotalTokens = 5
	check(t, e.Usage(oldUsage))
	check(t, e.store.compactOnline())
	after, err := e.Get(first.ID)
	check(t, err)
	if after.RequestID != "new" || after.RequestBody != before.RequestBody || after.ResponseBody != before.ResponseBody || after.ConversationHistory != before.ConversationHistory || after.Tokens.TotalTokens != 100 || len(after.UsageAttempts) != 1 {
		t.Fatal("old usage overwrote the latest request", after.Summary)
	}
	if totals := after.ConversationTotals; totals.TotalTokens != 165 || totals.TokenRequests != 2 || totals.DurationRequests != 2 || totals.DurationMS != 2000 {
		t.Fatal("old usage missing or counted repeatedly", totals)
	}
	storageAfter, err := e.store.StorageStats()
	check(t, err)
	if storageBefore["body_stored_bytes"] != storageAfter["body_stored_bytes"] || storageBefore["body_chunks"] != storageAfter["body_chunks"] {
		t.Fatal("late accounting rewrote conversation bodies")
	}
	if len(e.pendingUsage) != 0 || e.Status()["discarded_usage"] != uint64(0) {
		t.Fatal("late usage remained unmatched")
	}
}

func TestUsageReusedTraceMatchesExecutionIntervalAndKnownReporter(t *testing.T) {
	e := testEngine(t)
	start := time.Now().Add(-time.Minute)
	recordedUsageTurn(t, e, "first", "reused-trace", start)
	first, err := e.Get("first")
	check(t, err)
	recordedUsageTurn(t, e, "second", "reused-trace", start.Add(2*time.Second))
	check(t, e.Usage(wire.Usage{RequestID: "report-first", TraceID: "reused-trace", Model: "model", RequestedAt: start.Add(100 * time.Millisecond), Detail: wire.Tokens{TotalTokens: 10}}))
	check(t, e.Usage(wire.Usage{RequestID: "report-second", TraceID: "reused-trace", Model: "model", RequestedAt: start.Add(2100 * time.Millisecond), Detail: wire.Tokens{TotalTokens: 20}}))
	// A repeated report keeps its known owner even if optional context is absent.
	check(t, e.Usage(wire.Usage{RequestID: "report-first", Detail: wire.Tokens{TotalTokens: 15}}))
	r, err := e.Get(first.ID)
	check(t, err)
	if len(e.pendingUsage) != 0 || r.Tokens.TotalTokens != 20 || r.ConversationTotals.TotalTokens != 35 || r.ConversationTotals.TokenRequests != 2 {
		t.Fatal("trace reuse assigned usage to another turn", r.Summary)
	}
}

func TestUsageAliasMatchingAndUnknownModelRemainSafe(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			e := testEngine(t)
			check(t, e.Request(wire.Request{RequestID: "a", TraceID: "same-trace", Model: "upstream-a", RequestedModel: "public-a"}, false))
			secondModel := "upstream-b"
			if unknown {
				secondModel = ""
			}
			check(t, e.Request(wire.Request{RequestID: "b", TraceID: "same-trace", Model: secondModel}, false))
			check(t, e.Usage(wire.Usage{RequestID: "report", TraceID: "same-trace", Model: "served-a", Alias: "public-a", Detail: wire.Tokens{TotalTokens: 10}}))
			if unknown {
				if len(e.pendingUsage) != 1 || e.active["a"].record.Tokens != nil || e.active["b"].record.Tokens != nil {
					t.Fatal("guessed ownership with missing model metadata")
				}
			} else if len(e.pendingUsage) != 0 || e.active["a"].record.Tokens.TotalTokens != 10 || e.active["b"].record.Tokens != nil {
				t.Fatal("model alias was not used safely")
			}
		})
	}
}

func TestKnownUsageReporterSurvivesNewAmbiguousExecution(t *testing.T) {
	e := testEngine(t)
	check(t, e.Request(wire.Request{RequestID: "a", TraceID: "same-trace", Model: "model"}, false))
	u := wire.Usage{RequestID: "report-a", TraceID: "same-trace", Model: "model", Detail: wire.Tokens{TotalTokens: 10}}
	check(t, e.Usage(u))
	check(t, e.Request(wire.Request{RequestID: "b", TraceID: "same-trace", Model: "model"}, false))
	u.Detail.TotalTokens = 15
	check(t, e.Usage(u))
	if e.active["a"].record.Tokens.TotalTokens != 15 || e.active["b"].record.Tokens != nil || len(e.pendingUsage) != 0 {
		t.Fatal("known reporter became ambiguous")
	}
}

func TestEmptyAfterHookAndUsageCannotEraseLifecycleTrace(t *testing.T) {
	e := testEngine(t)
	check(t, e.Request(wire.Request{RequestID: "request", TraceID: "original", Model: "model"}, false))
	check(t, e.Request(wire.Request{RequestID: "request", Model: "model"}, true))
	check(t, e.Usage(wire.Usage{RequestID: "reporter", TraceID: "original", Model: "model", Detail: wire.Tokens{TotalTokens: 10}}))
	check(t, e.Usage(wire.Usage{RequestID: "request", TraceID: "alternate", Detail: wire.Tokens{TotalTokens: 5}}))
	if e.active["request"].record.TraceID != "original" || e.active["request"].record.Tokens.TotalTokens != 15 || len(e.pendingUsage) != 0 {
		t.Fatal("lifecycle trace was erased or replaced")
	}
}

func TestLongRequestUsageWaitsFromReceiptAndWarningDoesNotHideStorageErrors(t *testing.T) {
	e := testEngine(t)
	now := time.Now()
	u := wire.Usage{RequestID: "slow-report", TraceID: "slow-trace", Model: "model", RequestedAt: now.Add(-10 * time.Minute), Detail: wire.Tokens{TotalTokens: 30}}
	check(t, e.Usage(u))
	e.mu.Lock()
	e.expireUsage(now.Add(time.Second))
	stillPending := len(e.pendingUsage)
	e.mu.Unlock()
	if stillPending != 1 {
		t.Fatal("a long request expired immediately on receipt")
	}
	check(t, e.Request(wire.Request{RequestID: "slow", TraceID: "slow-trace", Model: "model"}, false))
	check(t, e.Complete(wire.Completion{RequestID: "slow", Outcome: "succeeded", StartedAt: u.RequestedAt.Add(-time.Second), CompletedAt: now}))
	e.mu.Lock()
	e.drainUsage()
	e.mu.Unlock()
	r, err := e.Get("slow")
	check(t, err)
	if r.Tokens.TotalTokens != 30 || len(e.pendingUsage) != 0 {
		t.Fatal("delayed lifecycle did not recover usage")
	}
	// Missing RequestedAt must not keep an orphan in the queue forever.
	check(t, e.Usage(wire.Usage{RequestID: "orphan"}))
	e.mu.Lock()
	e.lastError = "storage error"
	e.expireUsage(time.Now().Add(pendingUsageTTL + time.Second))
	e.mu.Unlock()
	status := e.Status()
	if status["last_error"] != "storage error" || status["discarded_usage"] != uint64(1) || !strings.Contains(fmt.Sprint(status["usage_warning"]), "token") {
		t.Fatal(status)
	}
	e.mu.Lock()
	e.lastUsageDrop = time.Now().Add(-pendingUsageTTL - time.Second)
	e.mu.Unlock()
	status = e.Status()
	if _, visible := status["usage_warning"]; visible || status["last_error"] != "storage error" {
		t.Fatal("old usage warning remained sticky or cleared a database error", status)
	}
}

func TestUsageCorrelationWindowIsBoundedAndExpires(t *testing.T) {
	e := testEngine(t)
	e.mu.Lock()
	for i := 0; i < maxRecentUsage+1; i++ {
		id := fmt.Sprint(i)
		e.rememberUsage(&Record{Summary: Summary{ID: "conversation", RequestID: id, TraceID: "trace-" + id}, UsageAttempts: []UsageAttempt{{ID: "report-" + id}}})
	}
	bounded := len(e.recentUsage) == maxRecentUsage && len(e.usageOwners) <= maxUsageOwners && len(e.recentUsageTraces) == maxRecentUsage
	e.expireUsage(time.Now().Add(recentUsageTTL + time.Second))
	empty := len(e.recentUsage) == 0 && len(e.usageOwners) == 0 && len(e.recentUsageTraces) == 0
	e.mu.Unlock()
	if !bounded || !empty {
		t.Fatal("correlation metadata leaked or did not expire")
	}
}
