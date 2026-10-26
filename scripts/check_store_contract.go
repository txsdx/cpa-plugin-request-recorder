//go:build ignore

// Run from the pinned CPA checkout (see check_store_contract.sh), keeping CPA
// dependencies out of the recorder's production module.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
)

type fixtureDoer struct {
	fallback *http.Client
	release  []byte
}

func (d fixtureDoer) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "api.github.com" && strings.HasSuffix(req.URL.Path, "/releases/latest") {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(d.release)), Request: req}, nil
	}
	return d.fallback.Do(req)
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	if len(os.Args) != 4 {
		panic("usage: check_store_contract.go REGISTRY ASSETS VERSION")
	}
	registryPath, assets, version := os.Args[1], os.Args[2], os.Args[3]
	registryData, err := os.ReadFile(registryPath)
	must(err)
	mux := http.NewServeMux()
	mux.HandleFunc("/registry.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(registryData)
	})
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(assets))))
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	targets := [][3]string{{"linux", "amd64", "so"}, {"linux", "arm64", "so"}, {"darwin", "amd64", "dylib"}, {"darwin", "arm64", "dylib"}, {"windows", "amd64", "dll"}}
	release := sdk.Release{TagName: "v" + version}
	for _, target := range targets {
		name := fmt.Sprintf("request-recorder_%s_%s_%s.zip", version, target[0], target[1])
		release.Assets = append(release.Assets, sdk.ReleaseAsset{Name: name, BrowserDownloadURL: server.URL + "/assets/" + name})
	}
	release.Assets = append(release.Assets, sdk.ReleaseAsset{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/assets/checksums.txt"})
	payload, err := json.Marshal(release)
	must(err)
	client := sdk.NewClient(fixtureDoer{server.Client(), payload}, server.URL+"/registry.json")
	ctx := context.Background()
	registry, err := client.FetchRegistry(ctx)
	must(err)
	if len(registry.Plugins) != 1 || registry.Plugins[0].ID != "request-recorder" {
		panic("wrong registry plugin identity")
	}
	plugin := registry.Plugins[0]
	derived, err := sdk.ReleaseVersion(release)
	must(err)
	if derived != version {
		panic("release version mismatch")
	}
	if sdk.UpdateAvailable(version, version) || !sdk.UpdateAvailable("0.0.0", version) {
		panic("unexpected update detection")
	}
	dir, err := os.MkdirTemp("", "cpa-store-contract-")
	must(err)
	defer os.RemoveAll(dir)
	for _, target := range targets {
		options := sdk.InstallOptions{PluginsDir: dir, GOOS: target[0], GOARCH: target[1]}
		result, err := client.Install(ctx, plugin, options)
		must(err)
		if result.Version != version || result.ReleaseTag != "v"+version {
			panic("wrong installed version")
		}
		if info, err := os.Stat(result.Path); err != nil || info.Size() == 0 {
			panic("library not installed")
		}
		rel, err := filepath.Rel(dir, result.Path)
		must(err)
		fmt.Printf("CPA installed %s/%s -> %s\n", target[0], target[1], rel)
		second, err := client.Install(ctx, plugin, options)
		must(err)
		if !second.Skipped {
			panic("identical reinstall should be skipped")
		}
	}
	// A wrong checksum must reject the same real archive before writing anything.
	mux.HandleFunc("/bad-checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  request-recorder_%s_linux_amd64.zip\n", strings.Repeat("0", 64), version)
	})
	release.Assets[len(release.Assets)-1].BrowserDownloadURL = server.URL + "/bad-checksums.txt"
	payload, err = json.Marshal(release)
	must(err)
	client = sdk.NewClient(fixtureDoer{server.Client(), payload}, server.URL+"/registry.json")
	_, err = client.Install(ctx, plugin, sdk.InstallOptions{PluginsDir: dir + "/tampered", GOOS: "linux", GOARCH: "amd64"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "checksum") {
		panic("checksum mismatch was not rejected")
	}
	fmt.Println("PASS: official CPA registry parser, all five platform installers, idempotent install, version/update detection, checksum rejection")
}
