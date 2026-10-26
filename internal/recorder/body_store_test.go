package recorder

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBodyStorageLosslessDedupAcrossShiftedHistoryAndImages(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "dedup.db"))
	check(t, err)
	defer s.Close()
	picture := make([]byte, 384<<10)
	_, err = rand.New(rand.NewSource(41)).Read(picture)
	check(t, err)
	base64Body := base64.StdEncoding.EncodeToString(picture)
	var logical int64
	for i := 0; i < 12; i++ {
		request := fmt.Sprintf(`{"prefix":%q,"image":"data:image/png;base64,%s"}`, strings.Repeat("prefix", i+1), base64Body)
		upstream := fmt.Sprintf("=== API REQUEST 1 ===\nBody:\n%s\n", request)
		response := strings.Repeat("data: {\"type\":\"response.output_text.delta\",\"delta\":\"完整文字\"}\n\n", 1000) + "data: [DONE]\n\n"
		r := &Record{Summary: Summary{ID: fmt.Sprint(i), StartedAt: time.Now()}, RequestBody: request, InterceptedBody: request, UpstreamRequests: upstream, ResponseBody: response, UpstreamResponses: response}
		logical += int64(3*len(request) + len(upstream) - len(request) + 2*len(response))
		check(t, s.Save(r))
		got, err := s.Get(r.ID)
		check(t, err)
		if got.RequestBody != request || got.InterceptedBody != request || got.UpstreamRequests != upstream || got.ResponseBody != response || got.UpstreamResponses != response {
			t.Fatal("body round trip changed original bytes")
		}
	}
	stats, err := s.StorageStats()
	check(t, err)
	stored := stats["body_stored_bytes"].(int64)
	if stored > logical/10 {
		t.Fatalf("dedup ineffective: logical=%d stored=%d", logical, stored)
	}
	t.Logf("Synthetic 12 requests with shifted shared base64/history and SSE: logical bodies=%d bytes, stored chunks=%d bytes (%.2f%%), database=%d bytes", logical, stored, 100*float64(stored)/float64(logical), stats["database_file_bytes"])
	before := stats["body_chunks"]
	r, err := s.Get("11")
	check(t, err)
	r.Model = "updated usage metadata"
	check(t, s.Save(r))
	after, err := s.StorageStats()
	check(t, err)
	if after["body_chunks"] != before || after["body_stored_bytes"] != stored {
		t.Fatal("metadata update duplicated body data")
	}
	check(t, s.Prune(0, 1))
	remaining, err := s.Get("11")
	check(t, err)
	if remaining.RequestBody != r.RequestBody {
		t.Fatal("pruning broke shared body references")
	}
	check(t, s.Prune(1, 0)) // recent record remains
	remaining.StartedAt = time.Now().AddDate(0, 0, -2)
	check(t, s.Save(remaining))
	check(t, s.Prune(1, 0))
	after, err = s.StorageStats()
	check(t, err)
	if after["body_chunks"] != 0 || after["records"] != 0 {
		t.Fatal("unreferenced chunks leaked", after)
	}
}
