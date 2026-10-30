package artifact

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/captain/ferry/internal/zipx"
)

// FileSums 附带文件里每个文件放到版本目录里的位置 → sha256（和服务器上记下的比，看哪些文件改了）。
// 读不了的文件跳过（部署时打包会报错）。
func FileSums(files []ExtraFile) map[string]string {
	out := map[string]string{}
	for _, x := range files {
		st, err := os.Stat(x.Local)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			if sum, _, err := zipx.FileSHA256(x.Local); err == nil {
				out[x.To] = sum
			}
			continue
		}
		filepath.WalkDir(x.Local, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(x.Local, p)
			if err != nil {
				return nil
			}
			if sum, _, err := zipx.FileSHA256(p); err == nil {
				out[x.To+"/"+filepath.ToSlash(rel)] = sum
			}
			return nil
		})
	}
	return out
}
