package recorder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	bolt "go.etcd.io/bbolt"
)

var fileFixture = []byte("%PDF-1.7\nsynthetic uploaded file\x00\xff\n%%EOF\n")

func fileData() string {
	return "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(fileFixture)
}
func fileRequest(name, data string) string {
	b, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "请检查这个附件"},
		map[string]any{"type": "file", "file": map[string]any{"filename": name, "file_data": data}},
	}}}})
	return string(b)
}
func awaitFiles(t *testing.T, s *Store, id string) []ImageAsset {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		assets, err := s.GetFiles(id)
		check(t, err)
		if len(assets) > 0 {
			pending := false
			for _, asset := range assets {
				pending = pending || asset.Status == "pending"
			}
			if !pending {
				return assets
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("file archive did not finish")
	return nil
}
func TestFileProtocolsAndOldHistoryNormalization(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(fileFixture)
	for _, tc := range []struct{ name, body string }{
		{"chat-data", fileRequest("notes.pdf", fileData())},
		{"chat-raw-base64", fileRequest("notes.pdf", b64)},
		{"responses", `{"input":[{"role":"user","content":[{"type":"input_file","filename":"notes.pdf","file_data":"` + fileData() + `"}]}]}`},
		{"claude", `{"messages":[{"role":"user","content":[{"type":"document","title":"notes.pdf","source":{"type":"base64","media_type":"application/pdf","data":"` + b64 + `"}}]}]}`},
		{"gemini", `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","displayName":"notes.pdf","data":"` + b64 + `"}}]}]}`},
		{"gemini-snake", `{"contents":[{"role":"user","parts":[{"inline_data":{"mime_type":"application/pdf","display_name":"notes.pdf","data":"` + b64 + `"}}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var found []File
			for _, m := range requestMessages(tc.body) {
				found = append(found, m.Files...)
				if len(m.Files) > 0 && m.Content != "" {
					t.Fatal("file payload leaked to readable content")
				}
			}
			if len(found) != 1 || found[0].Filename != "notes.pdf" {
				t.Fatal(found)
			}
			data, _, err := inlineFile(found[0].URL, 1024)
			check(t, err)
			if !bytes.Equal(data, fileFixture) {
				t.Fatal("file bytes changed")
			}
		})
	}
	for _, block := range []string{
		`{"type":"input_file","file_id":"file-provider-only"}`,
		`{"type":"document","source":{"type":"file","file_id":"file-provider-only"}}`,
	} {
		m := message("user", "attachment", block)
		normalizeMessageFiles(&m)
		if len(m.Files) != 1 || m.Files[0].Status != "unavailable" || m.Files[0].Reference != "file-provider-only" || m.Content != "" {
			t.Fatal(m)
		}
	}
	m := fileMessage("user", decode(`{"type":"document","title":"a.txt","source":{"type":"text","data":"中文 + exact text"}}`))
	data, _, err := inlineFile(m.Files[0].URL, 1024)
	check(t, err)
	if string(data) != "中文 + exact text" {
		t.Fatal(string(data))
	}
	old := &Record{ConversationHistory: `[{"role":"user","kind":"attachment","content":` + string(mustJSON(t, `{"type":"input_file","filename":"old.pdf","file_data":"`+fileData()+`"}`)) + `}]`}
	if got := conversationMessages(old); len(got) != 1 || len(got[0].Files) != 1 || got[0].Content != "" {
		t.Fatal(got)
	}
}
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	check(t, err)
	return b
}

func TestFileArchiveDownloadIntegrityMembershipPersistenceAndCleanup(t *testing.T) {
	e := testEngine(t)
	r := &Record{Summary: Summary{ID: "file-one", StartedAt: time.Now()}, RequestBody: fileRequest(`../../报告.pdf`, fileData())}
	check(t, e.store.Save(r))
	if r.FileCount != 1 {
		t.Fatal(r.FileCount)
	}
	e.files.enqueue(r.ID)
	assets := awaitFiles(t, e.store, r.ID)
	asset := assets[0]
	if asset.Status != "ready" || asset.Filename != "报告.pdf" || asset.Size != int64(len(fileFixture)) {
		t.Fatal(asset)
	}
	get := func(record, id string) wire.ManagementResponse {
		return e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/file", Query: url.Values{"record_id": {record}, "id": {id}}}, nil)
	}
	response := get(r.ID, asset.ID)
	_, params, err := mime.ParseMediaType(response.Headers.Get("Content-Disposition"))
	check(t, err)
	if response.StatusCode != 200 || !bytes.Equal(response.Body, fileFixture) || params["filename"] != "报告.pdf" || response.Headers.Get("Content-Type") != "application/octet-stream" || response.Headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal(response.Headers, response.StatusCode)
	}
	other := &Record{Summary: Summary{ID: "no-file", StartedAt: time.Now()}, RequestBody: `{"messages":[{"role":"user","content":"no attachment"}]}`}
	check(t, e.store.Save(other))
	if get(other.ID, asset.ID).StatusCode != 404 || get(r.ID, "../../etc/passwd").StatusCode != 400 {
		t.Fatal("membership/path guard failed")
	}
	chat := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/record", Query: url.Values{"id": {r.ID}, "view": {"chat"}}}, nil)
	if chat.StatusCode != 200 || bytes.Contains(chat.Body, []byte("base64,")) || !bytes.Contains(chat.Body, []byte("报告.pdf")) {
		t.Fatal(string(chat.Body))
	}
	check(t, e.files.close())
	e.files, err = newFileArchive(e.cfg, e.store)
	check(t, err)
	e.files.start()
	if got := get(r.ID, asset.ID); got.StatusCode != 200 || !bytes.Equal(got.Body, fileFixture) {
		t.Fatal("file lost across archive restart")
	}
	// Distinct source names keep their names, but share one physical byte blob.
	second := &Record{Summary: Summary{ID: "file-two", StartedAt: time.Now()}, RequestBody: fileRequest("copy.pdf", fileData())}
	check(t, e.store.Save(second))
	e.files.enqueue(second.ID)
	if copy := awaitFiles(t, e.store, second.ID)[0]; copy.Hash != asset.Hash || copy.ID == asset.ID {
		t.Fatal(copy)
	}
	e.files.filesMu.Lock()
	count := len(e.files.sizes)
	e.files.filesMu.Unlock()
	if count != 1 {
		t.Fatal("content deduplication failed", count)
	}
	check(t, os.WriteFile(filepath.Join(fileDirectory(e.cfg), asset.Hash+".bin"), []byte("broken"), 0600))
	if get(r.ID, asset.ID).StatusCode != 410 {
		t.Fatal("corruption was served")
	}
	check(t, e.files.retry(r.ID))
	if awaitFiles(t, e.store, r.ID)[0].Status != "ready" {
		t.Fatal("repair failed")
	}
	check(t, e.store.Prune(0, 0)) // zero means unlimited, neither reference is lost
	check(t, e.files.collect(time.Now().Add(time.Hour)))
	if get(second.ID, awaitFiles(t, e.store, second.ID)[0].ID).StatusCode != 200 {
		t.Fatal("shared file was collected")
	}
	check(t, e.store.Prune(0, 1))
	check(t, e.store.update(0, func(tx *bolt.Tx) error { return removeStoredRecord(tx, []byte(second.ID), second.Summary) }))
	check(t, e.files.collect(time.Now().Add(time.Hour)))
	if _, err := os.Stat(filepath.Join(fileDirectory(e.cfg), asset.Hash+".bin")); !os.IsNotExist(err) {
		t.Fatal("unreferenced file survived", err)
	}
}

func TestRemoteFilesUseSameTLSAndAddressGuards(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Error("credentials leaked")
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "https://127.0.0.1/private")
			w.WriteHeader(302)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(fileFixture)
	}))
	defer server.Close()
	f := fixtureFetcher(t, server)
	b, typ, err := f.fetchAsset(context.Background(), "https://example.com/report.pdf", 1024, true)
	check(t, err)
	if !bytes.Equal(b, fileFixture) || typ != "application/pdf" {
		t.Fatal("file download changed")
	}
	for _, source := range []string{"http://example.com/report.pdf", "https://example.com/redirect", "https://127.0.0.1/file", "file:///etc/passwd", "https://example.com:8443/file"} {
		if _, _, err := f.fetchAsset(context.Background(), source, 1024, true); err == nil {
			t.Fatal("unsafe file source accepted", source)
		}
	}
	if _, _, err := f.fetchAsset(context.Background(), "https://example.com/report.pdf", 8, true); err == nil {
		t.Fatal("file size limit ignored")
	}
	if _, _, err := inlineFile(fileData(), 8); err == nil {
		t.Fatal("inline size limit ignored")
	}
	if _, _, err := inlineFile("data:application/pdf;base64,invalid!", 1024); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	data, _, err := inlineFile("data:text/plain,a+b%20c", 1024)
	check(t, err)
	if string(data) != "a+b c" {
		t.Fatal("plus sign changed")
	}
	if name := safeFileName("..\\..\\报告\r\n.pdf"); strings.ContainsAny(name, "/\\\r\n") {
		t.Fatal(name)
	}
}

func TestForkFileReferencesSurviveParentRemoval(t *testing.T) {
	e := testEngine(t)
	parent := conversationTurn("fp1", "key", "parent", fileRequest("parent.pdf", fileData()), branchReply("resp_file_parent", "done"), 1)
	check(t, e.store.Save(parent))
	e.files.enqueue(parent.ID)
	asset := awaitFiles(t, e.store, parent.ID)[0]
	child := conversationTurn("fc1", "key", "child", `{"previous_response_id":"resp_file_parent","input":"follow up"}`, branchReply("resp_file_child", "next"), 2)
	check(t, e.store.Save(child))
	copy, err := e.store.GetFiles(child.ID)
	check(t, err)
	if len(copy) != 1 || copy[0].Hash != asset.Hash || child.FileCount != 1 {
		t.Fatal("branch lost archive", copy, child.FileCount)
	}
	check(t, e.store.update(0, func(tx *bolt.Tx) error { return removeStoredRecord(tx, []byte(parent.ID), parent.Summary) }))
	check(t, e.files.collect(time.Now().Add(time.Hour)))
	data, err := e.files.read(copy[0])
	check(t, err)
	if !bytes.Equal(data, fileFixture) {
		t.Fatal("branch bytes lost")
	}
}

func TestFileQuotasAndEmptyPayloads(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "records.db")
	cfg.MaxFileBytes, cfg.MaxFileStorageBytes, cfg.MaxCachedFiles = 1024, 1024, 1
	e, err := New(cfg)
	check(t, err)
	defer e.Close()
	for i, payload := range []string{fileData(), "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("different file"))} {
		r := &Record{Summary: Summary{ID: string(rune('a' + i)), StartedAt: time.Now()}, RequestBody: fileRequest("a.txt", payload)}
		check(t, e.store.Save(r))
		e.files.enqueue(r.ID)
		asset := awaitFiles(t, e.store, r.ID)[0]
		if (i == 0 && asset.Status != "ready") || (i == 1 && asset.Status != "failed") {
			t.Fatal(asset)
		}
	}
	empty, typ, err := inlineFile("data:application/octet-stream;base64,", 1024)
	check(t, err)
	if len(empty) != 0 || typ == "" {
		t.Fatal("empty file lost")
	}
}
