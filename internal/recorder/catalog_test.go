package recorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	bolt "go.etcd.io/bbolt"
)

func assertCatalogMatchesScan(t *testing.T, s *Store) {
	t.Helper()
	for _, outcome := range []string{"", "rejected"} {
		for _, ignored := range []bool{false, true} {
			for _, offset := range []int{0, 7, 50} {
				f := Filter{Outcome: outcome, IgnoreRejected: ignored, Limit: 5, Offset: offset}
				fast, err := s.List(f)
				check(t, err)
				f.Query = "conv_" // every fixture has this ID prefix; force a full scan
				scan, err := s.List(f)
				check(t, err)
				if !reflect.DeepEqual(fast, scan) {
					t.Fatalf("indexed totals/pagination differ from a scan: fast=%+v scan=%+v", fast, scan)
				}
			}
		}
	}
}

func TestCatalogTotalsAcrossReplacementPruneAndCompaction(t *testing.T) {
	s := conversationStore(t)
	var saved []*Record
	for i := 0; i < 25; i++ {
		r := conversationTurn(fmt.Sprint(i), "catalog-key", fmt.Sprint(i), `{"input":"question"}`, chatReply("answer"), i)
		r.Outcome = []string{"succeeded", "rejected", "failed", "incomplete"}[i%4]
		check(t, s.Save(r))
		saved = append(saved, r)
	}
	assertCatalogMatchesScan(t, s)
	saved[0].DurationMS = -1
	check(t, s.Save(saved[0]))
	assertCatalogMatchesScan(t, s)
	saved[0].DurationMS = 1
	check(t, s.Save(saved[0]))
	for i, r := range saved[:8] {
		r.Outcome = []string{"rejected", "succeeded"}[i%2]
		check(t, s.Save(r))
	}
	assertCatalogMatchesScan(t, s)
	check(t, s.Prune(0, 11))
	assertCatalogMatchesScan(t, s)
	check(t, s.compactOnline())
	assertCatalogMatchesScan(t, s)
}

func TestCatalogCacheRefreshesAliasesCurrentAndDelayedUsage(t *testing.T) {
	s := conversationStore(t)
	first := conversationTurn("first", "catalog-key", "a", `{"input":"question"}`, chatReply("answer"), 1)
	first.Tokens = &wire.Tokens{TotalTokens: 10}
	check(t, s.Save(first))
	page := func(f Filter) Page {
		t.Helper()
		raw, err := s.listJSON(f)
		check(t, err)
		var p Page
		check(t, json.Unmarshal(raw, &p))
		return p
	}
	raw, err := s.listJSON(Filter{})
	check(t, err)
	raw[0] = '!' // a caller must never be able to corrupt a cached response
	if got := page(Filter{}); got.Total != 1 || got.Items[0].ConversationTotals.TotalTokens != 10 {
		t.Fatal("bad cached page", got)
	}
	f := Filter{Alias: "panel"}
	if page(f).Total != 0 {
		t.Fatal("unexpected initial alias")
	}
	check(t, s.SyncPanelAliases(map[string]string{first.KeyID: "panel name"}))
	if page(f).Total != 1 {
		t.Fatal("panel alias did not invalidate the cached filter")
	}
	check(t, s.SetAlias(first.KeyID, "manual"))
	if page(f).Total != 0 {
		t.Fatal("manual override left a stale alias result")
	}
	check(t, s.SetAlias(first.KeyID, ""))
	if page(f).Total != 1 {
		t.Fatal("removing override did not restore panel alias")
	}
	_ = page(Filter{})
	first.Tokens = &wire.Tokens{TotalTokens: 20}
	check(t, s.Save(first))
	if page(Filter{}).Items[0].Tokens.TotalTokens != 20 {
		t.Fatal("current usage stayed cached")
	}
	next := conversationTurn("next", "catalog-key", "a", `{"input":"followup"}`, chatReply("answer"), 2)
	next.Tokens = &wire.Tokens{TotalTokens: 30}
	check(t, s.Save(next))
	if page(Filter{}).Items[0].RequestID != "next" {
		t.Fatal("new request stayed cached")
	}
	current, err := s.addDelayedUsage(next.ID, "first", 7, false)
	check(t, err)
	if current || page(Filter{}).Items[0].ConversationTotals.TotalTokens != 57 {
		t.Fatal("delayed usage stayed cached")
	}
	check(t, s.Prune(1, 0))
	if page(Filter{}).Total != 0 {
		t.Fatal("pruned rows stayed cached")
	}
}

func TestCatalogCountsRebuildAndTransactionRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	check(t, err)
	r := conversationTurn("r", "key", "session", `{"input":"question"}`, chatReply("answer"), 1)
	r.Outcome = "rejected"
	check(t, s.Save(r))
	check(t, s.update(0, func(tx *bolt.Tx) error { return tx.Bucket(storageMeta).Delete(catalogCountsKey) }))
	check(t, s.Close())
	s, err = Open(path)
	check(t, err)
	defer s.Close()
	assertCatalogMatchesScan(t, s)
	rolledBack := errors.New("rollback")
	err = s.update(0, func(tx *bolt.Tx) error {
		if err := removeStoredRecord(tx, []byte(r.ID), r.Summary); err != nil {
			return err
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatal(err)
	}
	assertCatalogMatchesScan(t, s)
	if page, err := s.List(Filter{}); err != nil || page.Total != 1 {
		t.Fatal("rollback lost row/count", page, err)
	}
}

func TestCatalogCacheConcurrentReadersSeeConsistentSnapshots(t *testing.T) {
	s := conversationStore(t)
	r := conversationTurn("r", "key", "session", `{"input":"question"}`, chatReply("answer"), 1)
	check(t, s.Save(r))
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Go(func() {
			for j := 0; j < 50; j++ {
				raw, err := s.listJSON(Filter{IgnoreRejected: true})
				if err != nil {
					t.Error(err)
					return
				}
				var page Page
				if err := json.Unmarshal(raw, &page); err != nil {
					t.Error(err)
					return
				}
				if page.Total != len(page.Items) || len(page.Items) > 1 {
					t.Errorf("mixed count/page snapshot: %+v", page)
					return
				}
				if len(page.Items) == 1 && page.Items[0].Outcome == "rejected" {
					t.Error("rejected row escaped filter")
					return
				}
			}
		})
	}
	for i := 0; i < 20; i++ {
		r.Outcome = []string{"rejected", "succeeded"}[i%2]
		check(t, s.Save(r))
	}
	readers.Wait()
	raw, err := s.listJSON(Filter{IgnoreRejected: true})
	check(t, err)
	var page Page
	check(t, json.Unmarshal(raw, &page))
	if page.Total != 1 || page.Items[0].Outcome != "succeeded" {
		t.Fatal("latest committed update stayed cached", page)
	}
}
