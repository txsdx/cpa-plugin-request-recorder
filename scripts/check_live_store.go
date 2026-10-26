//go:build ignore

// Verify the published private GitHub store with the same scoped credential
// rules documented for CPA. Credentials are consumed by the official SDK and
// are never printed or copied into configuration files.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	sdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
)

func main() {
	if len(os.Args) != 2 {
		panic("usage: check_live_store.go EXPECTED_VERSION|latest")
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		panic("GITHUB_TOKEN must be supplied by the CI job for private-store verification")
	}
	registryURL := "https://raw.githubusercontent.com/txsdx/cpa-plugin-request-recorder/main/registry.json"
	auth := []sdk.AuthConfig{
		{Match: "https://raw.githubusercontent.com/txsdx/cpa-plugin-request-recorder/", ApplyTo: []string{sdk.RequestKindRegistry}, Type: sdk.AuthTypeGitHubToken, TokenEnv: "GITHUB_TOKEN"},
		{Match: "https://api.github.com/repos/txsdx/cpa-plugin-request-recorder/", ApplyTo: []string{sdk.RequestKindMetadata, sdk.RequestKindArtifact}, Type: sdk.AuthTypeGitHubToken, TokenEnv: "GITHUB_TOKEN"},
	}
	client := sdk.NewClientWithAuth(&http.Client{Timeout: 90 * time.Second}, registryURL, auth)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	registry, err := client.FetchRegistry(ctx)
	mustLive(err)
	if len(registry.Plugins) != 1 || registry.Plugins[0].ID != "request-recorder" {
		panic("wrong live registry identity")
	}
	plugin := registry.Plugins[0]
	release, err := client.FetchLatestRelease(ctx, plugin)
	mustLive(err)
	version, err := sdk.ReleaseVersion(release)
	mustLive(err)
	if expected := os.Args[1]; expected != "latest" && expected != version {
		panic(fmt.Sprintf("latest release is %s; expected %s", version, expected))
	}
	dir, err := os.MkdirTemp("", "cpa-live-store-")
	mustLive(err)
	defer os.RemoveAll(dir)
	result, err := client.Install(ctx, plugin, sdk.InstallOptions{PluginsDir: dir, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH})
	mustLive(err)
	if result.Version != version {
		panic("installed version differs from latest")
	}
	if stat, err := os.Stat(result.Path); err != nil || stat.Size() == 0 {
		panic("live library not installed")
	}
	fmt.Printf("PASS: authenticated live registry -> latest %s -> checksum-verified real CPA install (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
}
func mustLive(err error) {
	if err != nil {
		panic(err)
	}
}
