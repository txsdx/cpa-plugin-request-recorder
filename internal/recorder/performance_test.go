package recorder

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	bolt "go.etcd.io/bbolt"
)

// Synthetic fixtures contain no real requests, credentials, or image URLs.
func benchmarkCatalog(b *testing.B, count int) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "catalog.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	err = s.update(0, func(tx *bolt.Tx) error {
		for i := 0; i < count; i++ {
			r := Summary{ID: fmt.Sprintf("record-%06d", i), KeyID: fmt.Sprintf("%064x", i%10), Model: "benchmark-model", Outcome: "succeeded", StartedAt: time.Unix(1_700_000_000+int64(i), 0), LatestPrompt: strings.Repeat("synthetic user prompt ", 20), LatestResponse: strings.Repeat("synthetic model answer ", 40)}
			if i%10 == 0 {
				r.Outcome = "rejected"
			}
			raw, _ := json.Marshal(r)
			if err := tx.Bucket(summaries).Put([]byte(r.ID), raw); err != nil {
				return err
			}
			if err := tx.Bucket(ordering).Put(orderKey(r), []byte(r.ID)); err != nil {
				return err
			}
		}
		var counts [24]byte
		binary.BigEndian.PutUint64(counts[:8], uint64(count))
		binary.BigEndian.PutUint64(counts[8:16], uint64((count+9)/10))
		return tx.Bucket(storageMeta).Put([]byte("catalog_counts"), counts[:])
	})
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// Bypass the response cache: this also measures refresh cost while new requests
// continuously change the catalog. Both totals and pagination must stay exact.
func BenchmarkCatalogUncached(b *testing.B) {
	s := benchmarkCatalog(b, 10000)
	for _, rejected := range []bool{false, true} {
		b.Run(fmt.Sprintf("ignore_rejected_%t", rejected), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.List(Filter{IgnoreRejected: rejected}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCatalogRefresh(b *testing.B) {
	s := benchmarkCatalog(b, 10000)
	e := &Engine{store: s, cfg: DefaultConfig()}
	for _, tc := range []struct{ name, query string }{{"all", ""}, {"ignore_rejected", "ignore_rejected=true"}, {"search", "q=synthetic"}} {
		b.Run(tc.name, func(b *testing.B) {
			query, _ := url.ParseQuery(tc.query)
			req := wire.ManagementRequest{Method: "GET", Path: APIBase + "/records", Query: query}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if response := e.Management(req, nil); response.StatusCode != 200 {
					b.Fatal(response.StatusCode)
				}
			}
		})
	}
}

func benchmarkLongRecord(b *testing.B) (*Store, string) {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "long.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	var text strings.Builder
	for text.Len() < 1<<20 {
		fmt.Fprintf(&text, "Synthetic paragraph %08d: the user and model retain complete conversation text.\n", text.Len())
	}
	r := &Record{Summary: Summary{ID: "long-record", KeyID: fingerprint("synthetic-benchmark-key"), StartedAt: time.Now(), Outcome: "succeeded"}, RequestHeaders: map[string][]string{"X-Session-Id": {"benchmark-session"}}, RequestBody: chatRequest(chat("user", text.String())), ResponseBody: branchReply("resp_root", text.String())}
	if err := s.Save(r); err != nil {
		b.Fatal(err)
	}
	if err := s.SetImages(r.ID, []ImageAsset{}); err != nil {
		b.Fatal(err)
	}
	return s, r.ID
}

func BenchmarkConversationSave(b *testing.B) {
	s, id := benchmarkLongRecord(b)
	r, err := s.Get(id)
	if err != nil {
		b.Fatal(err)
	}
	// Regenerate at a recorded boundary, keeping history size stable even when
	// the benchmark calibrator changes b.N. User-only incremental input appends.
	r.RequestBody = `{"previous_response_id":"resp_root","input":"regenerated followup"}`
	response := decode(r.ResponseBody)
	response["id"] = "resp_followup"
	body, _ := json.Marshal(response)
	r.ResponseBody = string(body)
	r.RequestID = "initial-followup"
	r.StartedAt = r.StartedAt.Add(time.Millisecond)
	if err := s.Save(r); err != nil {
		b.Fatal(err)
	}
	historySize := len(r.ConversationHistory)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.RequestID = fmt.Sprintf("regeneration-%d", i)
		r.StartedAt = r.StartedAt.Add(time.Millisecond)
		if err := s.Save(r); err != nil {
			b.Fatal(err)
		}
		if len(r.ConversationHistory) != historySize {
			b.Fatal("benchmark history must stay fixed in size")
		}
	}
}

func BenchmarkRecordRead(b *testing.B) {
	s, id := benchmarkLongRecord(b)
	e := &Engine{store: s, cfg: DefaultConfig(), images: &imageArchive{}}
	for _, view := range []string{"full", "chat"} {
		b.Run(view, func(b *testing.B) {
			query := url.Values{"id": {id}}
			if view == "chat" {
				query.Set("view", "chat")
			}
			req := wire.ManagementRequest{Method: "GET", Path: APIBase + "/record", Query: query}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if response := e.Management(req, nil); response.StatusCode != 200 {
					b.Fatal(response.StatusCode)
				}
			}
		})
	}
}
