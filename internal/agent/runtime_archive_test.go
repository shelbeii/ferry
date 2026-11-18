package agent

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeArchivePaths(t *testing.T) {
	const node = "node-v24.0.0-linux-x64"
	for _, tc := range []struct {
		name     string
		items    []tar.Header
		wantFile string
	}{
		{"unix-mode-and-strip", []tar.Header{{Name: node + "/bin/node", Typeflag: tar.TypeReg, Mode: 0o7755}}, "bin/node"},
		{"traversal", []tar.Header{{Name: node + "/../../outside", Typeflag: tar.TypeReg}}, ""},
		{"absolute", []tar.Header{{Name: "/tmp/outside", Typeflag: tar.TypeReg}}, ""},
		{"outside-symlink", []tar.Header{{Name: node + "/bin/npm", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}}, ""},
		{"outside-hardlink", []tar.Header{{Name: node + "/bin/npm", Typeflag: tar.TypeLink, Linkname: "/tmp/outside"}}, ""},
		{"reserved-marker", []tar.Header{{Name: node + "/" + managedRuntimeMarker, Typeflag: tar.TypeReg}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "runtime.tar.gz")
			out, err := os.Create(file)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(out)
			tw := tar.NewWriter(gz)
			for i := range tc.items {
				if err := tw.WriteHeader(&tc.items[i]); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			err = extractManagedRuntime(file, dest, "node")
			if (err == nil) != (tc.wantFile != "") {
				t.Fatalf("extract: %v", err)
			}
			if tc.wantFile != "" {
				st, err := os.Stat(filepath.Join(dest, tc.wantFile))
				if err != nil || !st.Mode().IsRegular() || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
					t.Fatalf("extracted file: %v, %v", st, err)
				}
			}
		})
	}
}

func TestDotnetFrameworkCompatibility(t *testing.T) {
	for _, tc := range []struct {
		have, want, rule string
		ok               bool
	}{
		{"10.0.15", "10.0.0", "", true},
		{"8.0.34", "10.0.0", "Major", false},
		{"11.0.0", "10.0.0", "Minor", false},
		{"11.0.0", "10.0.0", "Major", true},
		{"10.1.0", "10.0.0", "LatestPatch", false},
		{"10.0.15", "10.0.0", "Disable", false},
	} {
		if got := dotnetVersionMatches(tc.have, tc.want, tc.rule); got != tc.ok {
			t.Errorf("%+v: %v", tc, got)
		}
	}
}
