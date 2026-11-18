package rundist

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPythonAssetSelection(t *testing.T) {
	release := func(version, date, arch, variant, file string) string {
		t.Helper()
		b, err := json.Marshal(map[string]any{
			"version": version, "date": date, "artifacts": []map[string]string{{
				"platform": arch + "-unknown-linux-gnu", "variant": variant, "archive_format": "tar.gz",
				"url": "https://example.com/" + file + ".tar.gz", "sha256": strings.Repeat("a", 64),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	data := strings.Join([]string{
		release("3.14.5rc1+20261201", "2026-12-01", "aarch64", "install_only_stripped", "preview"),
		release("3.14.4+20261201", "2026-12-01", "aarch64", "freethreaded+install_only", "free-threaded"),
		release("3.14.3+20261201", "2026-12-01", "x86_64", "install_only", "wrong-arch"),
		release("3.14.3+20261101", "2026-11-01", "aarch64", "install_only", "older-build"),
		release("3.14.3+20261110", "2026-11-10", "aarch64", "install_only_stripped", "selected"),
		release("3.14.2+20261101", "2026-11-01", "aarch64", "install_only_stripped", "older-patch"),
	}, "\n")
	asset, err := pythonAsset([]byte(data), Selection{Kind: "python", Channel: "3.14", Arch: "arm64"})
	if err != nil || asset.Version != "3.14.3" || !strings.HasSuffix(asset.URL, "selected.tar.gz") {
		t.Fatalf("asset = %+v, %v", asset, err)
	}
	asset, err = pythonAsset([]byte(data), Selection{Kind: "python", Version: "3.14.2", Arch: "arm64"})
	if err != nil || asset.Version != "3.14.2" {
		t.Fatalf("exact asset = %+v, %v", asset, err)
	}
}

func TestRuntimeSelection(t *testing.T) {
	for _, tc := range []struct {
		choice Selection
		valid  bool
	}{
		{Selection{Kind: "node", Channel: "24", Arch: "arm64"}, true},
		{Selection{Kind: "python", Channel: "3.14", Arch: "x64"}, true},
		{Selection{Kind: "dotnet", Component: "aspnetcore", Version: "10.0.15", Arch: "x64"}, true},
		{Selection{Kind: "node", Channel: "24.0", Arch: "x64"}, false},
		{Selection{Kind: "dotnet", Channel: "10.0", Component: "native", Arch: "x64"}, false},
		{Selection{Kind: "node", Channel: "24", Arch: "i386"}, false},
		{Selection{Kind: "python", Version: "../bad", Arch: "x64"}, false},
	} {
		if err := tc.choice.Validate(); (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc.choice, err)
		}
	}
	choice := Selection{Kind: "dotnet", Version: "10.0.15", Arch: "x64"}
	if err := choice.Validate(); err != nil || choice.ID() != "dotnet-aspnetcore-10.0.15-x64" {
		t.Fatalf("default .NET component: %+v, %v", choice, err)
	}
}
