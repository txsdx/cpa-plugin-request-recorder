package recorder

import (
	"testing"
	"time"
)

func TestPanelAliasesSyncPreservesManualOverridesAndFiltersHistory(t *testing.T) {
	e := testEngine(t)
	keyID := fingerprint("panel-source-key")
	check(t, e.store.Save(&Record{Summary: Summary{ID: "panel-history", KeyID: keyID, StartedAt: time.Now()}}))
	check(t, e.store.SyncPanelAliases(map[string]string{keyID: "CPAMC 原备注"}))
	page, err := e.List(Filter{Alias: "CPAMC 原备注"})
	check(t, err)
	if page.Total != 1 {
		t.Fatal("automatic label missing from history/filter", page)
	}
	check(t, e.Alias(keyID, "", "手动别名"))
	check(t, e.store.SyncPanelAliases(map[string]string{keyID: "CPAMC 新备注"}))
	r, err := e.Get("panel-history")
	check(t, err)
	if r.KeyAlias != "手动别名" {
		t.Fatal("automatic sync overwrote explicit alias")
	}
	check(t, e.Alias(keyID, "", ""))
	r, err = e.Get("panel-history")
	check(t, err)
	if r.KeyAlias != "CPAMC 新备注" {
		t.Fatal("clearing manual alias did not restore synced name")
	}
	if err := e.store.SyncPanelAliases(map[string]string{"not-a-hash": "invalid"}); err == nil {
		t.Fatal("invalid fingerprint accepted")
	}
	r, err = e.Get("panel-history")
	check(t, err)
	if r.KeyAlias != "CPAMC 新备注" {
		t.Fatal("invalid snapshot partially replaced names")
	}
	check(t, e.store.SyncPanelAliases(map[string]string{}))
	r, err = e.Get("panel-history")
	check(t, err)
	if r.KeyAlias != "" {
		t.Fatal("removed CPAMC note persisted")
	}
}
