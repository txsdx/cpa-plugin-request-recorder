package recorder

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	bolt "go.etcd.io/bbolt"
)

func branchReply(id, text string) string {
	body := decode(chatReply(text))
	body["id"] = id
	raw, _ := json.Marshal(body)
	return string(raw)
}

func TestForkResponseBoundaryPreservesParentSiblingAndAccounting(t *testing.T) {
	s := conversationStore(t)
	first := conversationTurn("p1", "key", "parent", chatRequest(chat("user", "original question")), `{"id":"resp_p1","choices":[{"message":{"role":"assistant","reasoning_content":"original thought","content":"original answer"}}]}`, 1)
	first.Tokens = &wire.Tokens{TotalTokens: 100}
	check(t, s.Save(first))
	latest := conversationTurn("p2", "key", "parent", chatRequest(chat("user", "parent later question")), branchReply("resp_p2", "parent later answer"), 2)
	latest.Tokens = &wire.Tokens{TotalTokens: 200}
	check(t, s.Save(latest))
	parentHistory := latest.ConversationHistory
	var branches []*Record
	for i, session := range []string{"fork-a", "fork-b"} {
		r := conversationTurn(session, "key", session, `{"previous_response_id":"resp_p1","input":"`+session+` question"}`, branchReply("resp_"+session, session+" answer"), i+3)
		r.Tokens = &wire.Tokens{TotalTokens: 30}
		if i == 0 {
			// The parent advanced while this child request was in flight.
			r.StartedAt = first.StartedAt.Add(500 * time.Millisecond)
		}
		check(t, s.Save(r))
		branches = append(branches, r)
		if r.ID == latest.ID || r.ConversationTurns != 1 || r.ConversationTotals.TotalTokens != 30 || r.ConversationTotals.TokenRequests != 1 {
			t.Fatal("fork replaced parent or copied its accounting", r.Summary)
		}
		if r.Branch == nil || r.Branch.Status != "response" || r.Branch.ParentConversationID != latest.ID || r.Branch.ResponseID != "resp_p1" {
			t.Fatal("missing response boundary", r.Branch)
		}
		if !strings.Contains(r.ConversationHistory, "original thought") || strings.Contains(r.ConversationHistory, "parent later") || strings.Contains(r.ConversationHistory, "不能凭空") {
			t.Fatal("incorrect fork prefix", r.ConversationHistory)
		}
	}
	assertConversationCount(t, s, 3)
	got, err := s.Get(latest.ID)
	check(t, err)
	if got.ConversationHistory != parentHistory || branches[0].ID == branches[1].ID {
		t.Fatal("parent or sibling overwritten")
	}
	check(t, s.view(func(tx *bolt.Tx) error {
		owner := tx.Bucket(conversationKeys).Get([]byte(contextKey(fingerprint("key"), "response", "resp_p1")))
		if string(owner) != latest.ID {
			t.Fatal("inherited response stole its original owner's index")
		}
		return nil
	}))
	// A no-session continuation of an original response still reaches parent.
	parentRegen := conversationTurn("p3", "key", "", `{"previous_response_id":"resp_p1","input":"parent regenerated followup"}`, branchReply("resp_p3", "new parent answer"), 5)
	check(t, s.Save(parentRegen))
	if parentRegen.ID != latest.ID || strings.Contains(parentRegen.ConversationHistory, "fork-a") {
		t.Fatal("response index routed a parent continuation into a child")
	}
	next := conversationTurn("a2", "key", "fork-a", `{"previous_response_id":"resp_fork-a","input":"child followup"}`, branchReply("resp_a2", "child new answer"), 6)
	next.Tokens = &wire.Tokens{TotalTokens: 40}
	check(t, s.Save(next))
	if next.ID != branches[0].ID || next.ConversationTotals.TotalTokens != 70 || next.Branch.ParentConversationID != latest.ID || len(next.InheritedResponseIDs) != 1 {
		t.Fatal("child continuation lost branch identity or counters", next.Summary)
	}
	for _, branch := range branches {
		got, err := s.Get(branch.ID)
		check(t, err)
		if !strings.Contains(got.ConversationHistory, "original answer") || strings.Contains(got.ConversationHistory, "new parent answer") {
			t.Fatal("parent regeneration changed child's saved prefix")
		}
	}
}

func TestForkIdentityAndParentExtractionMatchCPA(t *testing.T) {
	for _, tc := range []struct {
		name, body, host, hostParent, child, parent string
		headers                                     http.Header
		fork, hostFork                              bool
	}{
		{name: "codex fork header still points to parent", headers: http.Header{"Session-Id": {"fork"}, "Thread-Id": {"parent"}, "X-Codex-Turn-Metadata": {`{"forked_from_thread_id":"parent","agent_name":"helper"}`}}, child: "client:fork", parent: "client:parent", fork: true},
		{name: "codex fork child thread", headers: http.Header{"Session-Id": {"parent"}, "Thread-Id": {"fork"}}, body: `{"metadata":{"forked_from_thread_id":"parent"}}`, child: "client:fork", parent: "client:parent", fork: true},
		{name: "ambiguous fork identity", headers: http.Header{"Session-Id": {"parent"}}, body: `{"forked_from_thread_id":"parent"}`, parent: "client:parent", fork: true},
		{name: "codex child thread", headers: http.Header{"Session-Id": {"parent"}, "Thread-Id": {"child"}}, child: "client:child", parent: "client:parent"},
		{name: "codex named agent", headers: http.Header{"Session-Id": {"parent"}, "X-Codex-Turn-Metadata": {`{"agent_name":"/root/helper"}`}}, child: "client:parent:agent:helper", parent: "client:parent"},
		{name: "generic agent", body: `{"session_id":"root","metadata":{"agent_id":"worker"}}`, child: "client:root:agent:worker", parent: "client:root"},
		{name: "claude nested parent agent", body: `{"metadata":{"user_id":"{\"session_id\":\"root\",\"agent_id\":\"worker\",\"parent_agent_id\":\"manager\"}"}}`, child: "client:claude:root:agent:worker", parent: "client:claude:root:agent:manager"},
		{name: "generic fork source", body: `{"session_id":"child","forkSource":{"sessionId":"root"}}`, child: "client:child", parent: "client:root", fork: true},
		{name: "ambiguous generic fork", body: `{"session_id":"root","forkSource":{"sessionId":"root"}}`, parent: "client:root", fork: true},
		{name: "billing parent", host: "codex:child", hostParent: "codex:parent", child: "client:child", parent: "client:parent"},
		{name: "derived routing is not client fork", host: "derived:child", hostParent: "derived:parent", hostFork: true},
		{name: "self loop", host: "codex:child", hostParent: "codex:child", child: "client:child"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Record{Summary: Summary{SessionID: tc.host, ParentSessionID: tc.hostParent, IsFork: tc.hostFork}, RequestBody: tc.body, RequestHeaders: tc.headers}
			child := clientConversationID(r)
			parent, fork := parentConversationIdentity(r, child)
			if child != tc.child || parent != tc.parent || fork != tc.fork {
				t.Fatalf("got child=%q parent=%q fork=%v", child, parent, fork)
			}
		})
	}
}

func TestNestedForkKeepsDeclaredParentAndOriginalResponseOwner(t *testing.T) {
	s := conversationStore(t)
	root := conversationTurn("root", "key", "root", `{"input":"root question"}`, branchReply("resp_root", "root answer"), 1)
	check(t, s.Save(root))
	child := conversationTurn("child", "key", "child", `{"previous_response_id":"resp_root","input":"child question"}`, branchReply("resp_child", "child answer"), 2)
	check(t, s.Save(child))
	grandchild := conversationTurn("grandchild", "key", "grandchild", `{"parent_session_id":"child","previous_response_id":"resp_root","input":"grandchild question"}`, branchReply("resp_grandchild", "grandchild answer"), 3)
	check(t, s.Save(grandchild))
	if grandchild.Branch == nil || grandchild.Branch.ParentConversationID != child.ID || grandchild.Branch.Status != "response" {
		t.Fatal("nested fork lost its declared parent", grandchild.Branch)
	}
	if !strings.Contains(grandchild.ConversationHistory, "root answer") || strings.Contains(grandchild.ConversationHistory, `"content":"child question"`) {
		t.Fatal("nested fork inherited beyond the response boundary", grandchild.ConversationHistory)
	}
	check(t, s.view(func(tx *bolt.Tx) error {
		owner := tx.Bucket(conversationKeys).Get([]byte(contextKey(fingerprint("key"), "response", "resp_root")))
		if string(owner) != root.ID {
			t.Fatal("nested fork stole root response lookup")
		}
		return nil
	}))
	assertConversationCount(t, s, 3)
}

func TestParentIDAloneDoesNotCopyUnobservedContext(t *testing.T) {
	s := conversationStore(t)
	parent := conversationTurn("p", "key", "parent", chatRequest(chat("user", "private parent question")), branchReply("resp_parent", "parent answer"), 1)
	parent.CallerScope = "caller-a"
	check(t, s.Save(parent))
	for i, tc := range []struct{ scope, session, anchor, status string }{
		{"caller-a", "child", "", "linked"},
		{"caller-a", "missing-point", "never-recorded", "missing"},
		{"caller-b", "other-caller", "resp_parent", "missing"},
	} {
		body := map[string]any{"session_id": tc.session, "parent_session_id": "parent", "input": "independent task"}
		if tc.anchor != "" {
			body["previous_response_id"] = tc.anchor
		}
		raw, _ := json.Marshal(body)
		r := conversationTurn(fmt.Sprint(i), "key", "", string(raw), chatReply("child answer"), i+2)
		r.CallerScope = tc.scope
		check(t, s.Save(r))
		if r.Branch == nil || r.Branch.Status != tc.status || strings.Contains(r.ConversationHistory, "private parent") {
			t.Fatal("invented parent context or crossed caller scope", r.Branch)
		}
		if tc.scope != parent.CallerScope && r.Branch.ParentConversationID != "" {
			t.Fatal("cross-caller parent link")
		}
	}
	assertConversationCount(t, s, 4)
}

func TestLateParentMetadataRecoversConfirmedPrefixOnlyOnce(t *testing.T) {
	s := conversationStore(t)
	parent := conversationTurn("p", "key", "parent", chatRequest(chat("user", "question")), `{"id":"resp_parent","choices":[{"message":{"role":"assistant","reasoning_content":"saved thought","content":"answer"}}]}`, 1)
	check(t, s.Save(parent))
	child := conversationTurn("c", "key", "child", chatRequest(chat("user", "question"), chat("assistant", "answer"), chat("user", "child task")), chatReply("child answer"), 2)
	check(t, s.Save(child))
	if child.Branch != nil {
		t.Fatal("invented a parent before it was reported")
	}
	child.ParentSessionID = "codex:parent"
	check(t, s.Save(child))
	if child.Branch == nil || child.Branch.Status != "context" || !strings.Contains(child.ConversationHistory, "saved thought") || child.ConversationTurns != 1 {
		t.Fatal("late parent did not restore the confirmed prefix", child.Summary)
	}
	before := child.ConversationHistory
	check(t, s.Save(child))
	if child.ConversationHistory != before || child.ConversationTurns != 1 {
		t.Fatal("repeated metadata duplicated branch history")
	}
}

func TestInheritedImageAndHistorySurviveParentPruneAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "branches.db")
	s, err := Open(path)
	check(t, err)
	imageURL := "https://images.example/archived.png"
	parent := conversationTurn("p", "key", "parent", `{"input":[{"role":"user","content":[{"type":"input_text","text":"image question"},{"type":"input_image","image_url":"`+imageURL+`"}]}]}`, branchReply("resp_image", "image answer"), 1)
	check(t, s.Save(parent))
	asset := ImageAsset{ID: imageID(imageURL), Status: "ready", Hash: "local-image-hash", MIME: "image/png"}
	check(t, s.SetImages(parent.ID, []ImageAsset{asset, {ID: "not-inherited", Status: "ready", Hash: "later-image"}}))
	child := conversationTurn("c", "key", "child", `{"previous_response_id":"resp_image","input":"branch question"}`, branchReply("resp_child", "child answer"), 2)
	check(t, s.Save(child))
	images, err := s.GetImages(child.ID)
	check(t, err)
	if len(images) != 1 || images[0].Hash != asset.Hash {
		t.Fatal("did not copy exactly the inherited original's local reference", images)
	}
	check(t, s.Prune(0, 1))
	check(t, s.Close())
	s, err = Open(path)
	check(t, err)
	defer s.Close()
	got, err := s.Get(child.ID)
	check(t, err)
	if !strings.Contains(got.ConversationHistory, imageURL) || !strings.Contains(got.ConversationHistory, "image answer") || got.Branch.ParentConversationID != parent.ID || len(got.Images) != 1 {
		t.Fatal("pruning parent removed child's inherited data")
	}
}
