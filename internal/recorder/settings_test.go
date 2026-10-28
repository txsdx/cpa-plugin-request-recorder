package recorder

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

func TestIgnoreRejectedFiltersBeforePaginationAndPreservesRecords(t *testing.T) {
	e := testEngine(t)
	for i, outcome := range []string{"succeeded", "rejected", "failed", "rejected", "canceled", "incomplete", "rejected"} {
		check(t, e.store.Save(&Record{Summary: Summary{ID: fmt.Sprint(i), Outcome: outcome, StartedAt: time.Unix(int64(i), 0)}}))
	}
	list := func(query url.Values) Page {
		t.Helper()
		response := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/records", Query: query}, nil)
		if response.StatusCode != 200 {
			t.Fatalf("list failed: %d %s", response.StatusCode, response.Body)
		}
		var page Page
		check(t, json.Unmarshal(response.Body, &page))
		return page
	}
	if page := list(nil); page.Total != 7 {
		t.Fatal("default must include rejected entries", page)
	}
	response := e.Management(wire.ManagementRequest{Method: "POST", Path: APIBase + "/settings", Body: []byte(`{"ignore_rejected":true}`)}, nil)
	if response.StatusCode != 200 || e.Status()["ignore_rejected"] != true || e.Status()["refresh_interval_seconds"] != 1 {
		t.Fatal("preference failed to save without changing refresh interval", response.StatusCode, string(response.Body))
	}
	if page := list(url.Values{"offset": {"1"}, "limit": {"2"}}); page.Total != 4 || len(page.Items) != 2 || page.Items[0].ID != "4" || page.Items[1].ID != "2" {
		t.Fatal("must exclude only rejected entries before counting and pagination", page)
	}
	if page := list(url.Values{"outcome": {"rejected"}}); page.Total != 0 {
		t.Fatal("rejected status filter bypassed exclusion", page)
	}
	if page := list(url.Values{"ignore_rejected": {"false"}, "outcome": {"rejected"}}); page.Total != 3 {
		t.Fatal("explicit override must still expose recorded rejections", page)
	}
	if page := list(url.Values{"ignore_rejected": {"false"}}); page.Total != 7 {
		t.Fatal("toggling the filter deleted records", page)
	}
	bad := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/records", Query: url.Values{"ignore_rejected": {"invalid"}}}, nil)
	if bad.StatusCode != 400 {
		t.Fatal("invalid Boolean accepted", bad.StatusCode)
	}
}

func TestPartialSettingsAreAtomicAndSurviveReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	s, err := Open(path)
	check(t, err)
	interval, ignored := 3, true
	check(t, s.SetSettings(settingsUpdate{Interval: &interval}))
	check(t, s.SetSettings(settingsUpdate{IgnoreRejected: &ignored}))
	zero, disabled := 0, false
	if err := s.SetSettings(settingsUpdate{Interval: &zero, IgnoreRejected: &disabled}); err == nil {
		t.Fatal("invalid interval accepted")
	}
	if got := s.Settings(1); got.RefreshIntervalSeconds != 3 || !got.IgnoreRejected {
		t.Fatal("partial/invalid update changed unrelated settings", got)
	}
	check(t, s.Close())
	s, err = Open(path)
	check(t, err)
	defer s.Close()
	if got := s.Settings(1); got.RefreshIntervalSeconds != 3 || !got.IgnoreRejected {
		t.Fatal("settings lost after reopening", got)
	}
	check(t, s.SetSettings(settingsUpdate{IgnoreRejected: &disabled}))
	if got := s.Settings(1); got.RefreshIntervalSeconds != 3 || got.IgnoreRejected {
		t.Fatal("false was treated as an omitted field", got)
	}
}
