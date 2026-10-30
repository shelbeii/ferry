package artifact

import (
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// SiteManifest 静态站点目录里各文件的 sha256 与大小（路径相对 dir，用 /，排好序）。
func SiteManifest(dir string) ([]proto.SiteFile, error) {
	var files []proto.SiteFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		sha, size, err := zipx.FileSHA256(p)
		if err != nil {
			return err
		}
		files = append(files, proto.SiteFile{Path: filepath.ToSlash(rel), SHA256: sha, Size: size})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

// SiteChanges 和服务器上的版本比：要上传的文件（新的、变了的）与它们的总大小。
func SiteChanges(local, remote []proto.SiteFile) (map[string]bool, int64) {
	have := make(map[string]string, len(remote))
	for _, f := range remote {
		have[f.Path] = f.SHA256
	}
	changed := map[string]bool{}
	var bytes int64
	for _, f := range local {
		if have[f.Path] != f.SHA256 {
			changed[f.Path] = true
			bytes += f.Size
		}
	}
	return changed, bytes
}
