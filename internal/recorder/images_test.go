package recorder

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	"github.com/txsdx/cpa-plugin-request-recorder/web"
)

func imageFixture(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	m := image.NewRGBA(image.Rect(0, 0, 16, 12))
	m.Set(0, 0, color.RGBA{G: 255, A: 255})
	check(t, png.Encode(&b, m))
	return b.Bytes()
}
func inlineFixture(t *testing.T) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageFixture(t))
}
func recordWithImage(id, source string) *Record {
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]string{"url": source}}}}}})
	return &Record{Summary: Summary{ID: id, StartedAt: time.Now(), Outcome: "succeeded"}, RequestBody: string(body)}
}
func awaitAssets(t *testing.T, s *Store, id string) []ImageAsset {
	t.Helper()
	return awaitAssetsWithin(t, s, id, 5*time.Second)
}
func awaitAssetsWithin(t *testing.T, s *Store, id string, timeout time.Duration) []ImageAsset {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		assets, err := s.GetImages(id)
		check(t, err)
		if len(assets) > 0 {
			ready := true
			for _, a := range assets {
				if a.Status == "pending" {
					ready = false
				}
			}
			if ready {
				return assets
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("image work did not complete")
	return nil
}
func fixtureFetcher(t *testing.T, server *httptest.Server) *imageFetcher {
	t.Helper()
	transport := server.Client().Transport.(*http.Transport)
	dialer := &net.Dialer{}
	f := newImageFetcher()
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	f.proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	f.tlsConfig = transport.TLSClientConfig.Clone()
	f.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Errorf("dial was not pinned: %s", address)
		}
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	return f
}
func TestImageNetworkAddressGuards(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "192.168.0.1", "172.16.1.1", "0.0.0.0", "224.0.0.1", "240.0.0.1", "198.18.0.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::", "2001:db8::1", "3fff::1"} {
		if publicImageIP(netip.MustParseAddr(ip)) {
			t.Errorf("private/special IP allowed: %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicImageIP(netip.MustParseAddr(ip)) {
			t.Errorf("public IP rejected: %s", ip)
		}
	}
	for _, raw := range []string{"http://example.com/a.png", "file:///etc/passwd", "https://user:pass@example.com/a.png", "https://127.0.0.1/a.png", "https://example.com:8443/a.png", "https://[::1]/a.png", "https://example.com/a.png#part"} {
		if _, err := imageRemoteURL(raw); err == nil {
			t.Errorf("unsafe URL allowed: %s", raw)
		}
	}
}
func TestRemoteImagePinsDNSAndNeverForwardsCredentials(t *testing.T) {
	data := imageFixture(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Error("caller credentials forwarded")
		}
		if r.Host != "example.com" {
			t.Error("original Host lost")
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(data)
	}))
	defer server.Close()
	f := fixtureFetcher(t, server)
	b, mime, err := f.fetch(context.Background(), "https://example.com/image.png", 1024)
	check(t, err)
	if !bytes.Equal(b, data) || mime != "image/png" || calls.Load() != 1 {
		t.Fatal("download bytes/type mismatch")
	}
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	if _, _, err := f.fetch(context.Background(), "https://example.com/image.png", 1024); err == nil {
		t.Fatal("mixed public/private DNS allowed")
	}
	if calls.Load() != 1 {
		t.Fatal("unsafe DNS reached server")
	}
}
func TestImageRedirectAndContentLimits(t *testing.T) {
	data := imageFixture(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/private":
			w.Header().Set("Location", "https://127.0.0.1/secret")
			w.WriteHeader(302)
		case "/http":
			w.Header().Set("Location", "http://example.com/a")
			w.WriteHeader(302)
		case "/large":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 2000))
		case "/html":
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "<html><script>alert(1)</script></html>")
		case "/loop":
			w.Header().Set("Location", "/loop")
			w.WriteHeader(302)
		case "/slow":
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write(data)
		default:
			_, _ = w.Write(data)
		}
	}))
	defer server.Close()
	f := fixtureFetcher(t, server)
	for _, path := range []string{"private", "http", "large", "html", "loop"} {
		if _, _, err := f.fetch(context.Background(), "https://example.com/"+path, 1024); err == nil {
			t.Errorf("unsafe download accepted: %s", path)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := f.fetch(ctx, "https://example.com/slow", 1024); err == nil {
		t.Fatal("timeout ignored")
	}
	// Even an allowed public IP cannot use an untrusted source certificate.
	f.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if _, _, err := f.fetch(context.Background(), "https://example.com/image", 1024); err == nil {
		t.Fatal("TLS verification bypassed")
	}
}
func TestImageArchivePersistenceMembershipQuotaAndCleanup(t *testing.T) {
	e := testEngine(t)
	source := inlineFixture(t)
	r := recordWithImage("one", source)
	check(t, e.store.Save(r))
	e.images.enqueue(r.ID)
	assets := awaitAssets(t, e.store, r.ID)
	if len(assets) != 1 || assets[0].Status != "ready" {
		t.Fatal(assets)
	}
	original := imageFixture(t)
	archived, err := e.images.read(assets[0])
	check(t, err)
	if !bytes.Equal(original, archived) {
		t.Fatal("image was transformed")
	}
	path := filepath.Join(imageDirectory(e.cfg), assets[0].Hash+".png")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	second := recordWithImage("two", source)
	check(t, e.store.Save(second))
	e.images.enqueue(second.ID)
	other := awaitAssets(t, e.store, second.ID)
	if other[0].Hash != assets[0].Hash {
		t.Fatal("duplicate content was not shared")
	}
	saved, err := e.Get(r.ID)
	check(t, err)
	if !strings.Contains(saved.RequestBody, source) || len(saved.Images) != 1 {
		t.Fatal("raw body or metadata lost")
	}
	query := map[string][]string{"record_id": {"one"}, "id": {assets[0].ID}}
	resp := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: query}, web.Files)
	if resp.StatusCode != 200 || !bytes.Equal(resp.Body, original) || resp.Headers.Get("Content-Type") != "image/png" {
		t.Fatal(resp)
	}
	check(t, e.store.Save(&Record{Summary: Summary{ID: "no-image", StartedAt: time.Now()}}))
	query["record_id"] = []string{"no-image"}
	if got := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: query}, web.Files); got.StatusCode != 404 {
		t.Fatal("cross-record image access allowed")
	}
	if got := e.Management(wire.ManagementRequest{Method: "GET", Path: ResourceBase + "/image", Query: query}, web.Files); got.StatusCode != 404 {
		t.Fatal("public image endpoint exists")
	}
	messages := Conversation(saved.RequestBody, "")
	e.images.decorate(messages, saved.Images)
	if messages[0].Images[0].URL != "" || messages[0].Images[0].Status != "ready" {
		t.Fatal("external/inline source escaped to image renderer")
	}
	check(t, e.images.collect(time.Now().Add(time.Hour)))
	if _, err := os.Stat(path); err != nil {
		t.Fatal("referenced image collected")
	}
	// Pruning removes metadata; GC only deletes unreferenced owned files.
	check(t, e.store.Prune(0, 1))
	if _, err := e.store.GetImages("one"); err != ErrNotFound {
		t.Fatal("pruned record image metadata retained")
	}
	marker := filepath.Join(imageDirectory(e.cfg), "keep-user-file.txt")
	check(t, os.WriteFile(marker, []byte("keep"), 0600))
	check(t, e.images.collect(time.Now().Add(time.Hour)))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unreferenced image not collected")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("user file removed")
	}
	e.images.cfg.MaxImageStorageBytes = 8
	if _, err := e.images.put(original); err == nil {
		t.Fatal("quota ignored")
	}
}
func TestOldRecordImagesArchiveOnFirstView(t *testing.T) {
	e := testEngine(t)
	r := recordWithImage("old", inlineFixture(t))
	check(t, e.store.Save(r))
	resp := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/record", Query: map[string][]string{"id": {"old"}}}, web.Files)
	if resp.StatusCode != 200 {
		t.Fatal(resp)
	}
	if got := awaitAssets(t, e.store, "old"); got[0].Status != "ready" {
		t.Fatal(got)
	}
}

func TestProxyCONNECTPinsTargetAndKeepsProxyAuthOffOrigin(t *testing.T) {
	data := imageFixture(t)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Authorization") != "" {
			t.Error("proxy/caller auth leaked to origin")
		}
		_, _ = w.Write(data)
	}))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "8.8.8.8:443" {
			t.Errorf("CONNECT did not pin vetted IP: %s %s", r.Method, r.Host)
			http.Error(w, "bad", 400)
			return
		}
		if r.Header.Get("Proxy-Authorization") == "" {
			t.Error("proxy authentication missing")
		}
		remote, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			remote.Close()
			t.Error(err)
			return
		}
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffer.Flush()
		go func() { _, _ = io.Copy(remote, client); remote.Close(); client.Close() }()
		_, _ = io.Copy(client, remote)
		client.Close()
		remote.Close()
	}))
	defer proxy.Close()
	f := fixtureFetcher(t, origin)
	dialer := &net.Dialer{}
	f.dial = dialer.DialContext
	proxyURL, err := url.Parse(proxy.URL)
	check(t, err)
	proxyURL.User = url.UserPassword("proxy-user", "proxy-test-password")
	f.proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	got, _, err := f.fetch(context.Background(), "https://example.com/image.png", 1024)
	check(t, err)
	if !bytes.Equal(got, data) {
		t.Fatal("proxy changed original image bytes")
	}
}
func TestArchivedImageRepairAndDownloadFailureRemainPrivate(t *testing.T) {
	e := testEngine(t)
	r := recordWithImage("repair", inlineFixture(t))
	check(t, e.store.Save(r))
	e.images.enqueue(r.ID)
	assets := awaitAssets(t, e.store, r.ID)
	path := filepath.Join(imageDirectory(e.cfg), assets[0].Hash+".png")
	check(t, os.WriteFile(path, []byte("corrupted"), 0600))
	req := wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: map[string][]string{"record_id": {r.ID}, "id": {assets[0].ID}}}
	if got := e.Management(req, web.Files); got.StatusCode != 410 {
		t.Fatal("corrupted image served")
	}
	check(t, e.images.retry(r.ID))
	fixed := awaitAssets(t, e.store, r.ID)
	if fixed[0].Status != "ready" {
		t.Fatal(fixed)
	}
	if got := e.Management(req, web.Files); got.StatusCode != 200 {
		t.Fatal(got)
	}
	blocked := recordWithImage("blocked", "https://169.254.169.254/latest/meta-data/?secret-do-not-echo=123")
	check(t, e.store.Save(blocked))
	e.images.enqueue(blocked.ID)
	rejected := awaitAssets(t, e.store, blocked.ID)
	if rejected[0].Status != "failed" || strings.Contains(rejected[0].Error, "secret-do-not-echo") {
		t.Fatal(rejected)
	}
}
func TestRemoteArchiveRemainsViewableAfterOriginAndPluginRestart(t *testing.T) {
	data := imageFixture(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write(data) }))
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "persistent.db")
	e, err := New(cfg)
	check(t, err)
	e.images.fetcher = fixtureFetcher(t, server)
	r := recordWithImage("persistent", "https://example.com/expiring.png?signature=do-not-log")
	check(t, e.store.Save(r))
	e.images.enqueue(r.ID)
	assets := awaitAssets(t, e.store, r.ID)
	if assets[0].Status != "ready" {
		t.Fatal(assets)
	}
	check(t, e.Close())
	server.Close()
	restored, err := New(cfg)
	check(t, err)
	defer restored.Close()
	response := restored.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: map[string][]string{"record_id": {r.ID}, "id": {assets[0].ID}}}, web.Files)
	if response.StatusCode != 200 || !bytes.Equal(response.Body, data) || calls.Load() != 1 {
		t.Fatal("persistent archive depended on image origin")
	}
}

func TestImageQueueOverflowSurvivesRestart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "overflow.db")
	s, err := Open(cfg.DBPath)
	check(t, err)
	a, err := newImageArchive(cfg, s)
	check(t, err)
	source := inlineFixture(t)
	// Deliberately leave workers stopped and overflow the bounded memory queue.
	for i := 0; i < 258; i++ {
		id := fmt.Sprintf("record-%03d", i)
		check(t, s.Save(recordWithImage(id, source)))
		queued := a.enqueue(id)
		if i >= 256 && queued {
			t.Fatal("memory queue should be full")
		}
	}
	pending, err := s.PendingImageRecords(1000)
	check(t, err)
	if len(pending) != 258 {
		t.Fatal("overflow lost durable image jobs", len(pending))
	}
	check(t, a.close())
	check(t, s.Close())
	restored, err := New(cfg)
	check(t, err)
	defer restored.Close()
	if assets := awaitAssetsWithin(t, restored.store, "record-257", 45*time.Second); assets[0].Status != "ready" {
		t.Fatal("overflow image did not resume", assets)
	}
}
func TestImageDiscoveryIncludesClientRequestAndCPAResponse(t *testing.T) {
	r := recordWithImage("final", "https://example.com/request.png")
	r.ResponseBody = `{"data":[{"url":"https://example.com/response.png"}]}`
	if sources := recordImageSources(r, 64); len(sources) != 2 {
		t.Fatal(sources)
	}
}

func TestArchiveUsesPayloadExtensionAndRenamesExistingIMG(t *testing.T) {
	data := imageFixture(t)
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "existing.db")
	dir := imageDirectory(cfg)
	check(t, os.MkdirAll(dir, 0700))
	hash := imageBytesID(data)
	old := filepath.Join(dir, hash+".img")
	check(t, os.WriteFile(old, data, 0600))
	// A copy with the correct extension may already exist after a partial rename.
	check(t, os.WriteFile(filepath.Join(dir, hash+".png"), data, 0600))
	e, err := New(cfg)
	check(t, err)
	defer e.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal(".img was not renamed/deduplicated")
	}
	if e.images.used != int64(len(data)) || len(e.images.sizes) != 1 {
		t.Fatal("archive accounting duplicated files")
	}
	// The declared MIME is deliberately wrong: actual PNG bytes control the suffix.
	source := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)
	r := recordWithImage("actual-png", source)
	check(t, e.store.Save(r))
	e.images.enqueue(r.ID)
	assets := awaitAssets(t, e.store, r.ID)
	if assets[0].MIME != "image/png" || imageFileName(assets[0]) != hash+".png" {
		t.Fatal(assets)
	}
	actual, err := os.ReadFile(filepath.Join(dir, imageFileName(assets[0])))
	check(t, err)
	if !bytes.Equal(data, actual) {
		t.Fatal("rename transformed original bytes")
	}
}

func TestOriginalJPEGAndGIFExtensionsAndGCGrace(t *testing.T) {
	e := testEngine(t)
	m := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var jpg, gifData bytes.Buffer
	check(t, jpeg.Encode(&jpg, m, nil))
	check(t, gif.Encode(&gifData, m, nil))
	for _, fixture := range []struct {
		data []byte
		ext  string
	}{{jpg.Bytes(), ".jpg"}, {gifData.Bytes(), ".gif"}} {
		hash, err := e.images.put(fixture.data)
		check(t, err)
		path := filepath.Join(imageDirectory(e.cfg), hash+fixture.ext)
		actual, err := os.ReadFile(path)
		check(t, err)
		if !bytes.Equal(actual, fixture.data) {
			t.Fatal("image transcoded")
		}
		past := time.Now().Add(-time.Hour)
		check(t, os.Chtimes(path, past, past))
		_, err = e.images.put(fixture.data)
		check(t, err)
		check(t, e.images.collect(time.Now().Add(-5*time.Minute)))
		if _, err := os.Stat(path); err != nil {
			t.Fatal("deduplicated image lost before reference could be saved", err)
		}
	}
}
