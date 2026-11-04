package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/captain/ferry/internal/proto"
)

// 每次上传独占一个缓存目录；另一台电脑上传、部署或取消时不会删除本次要用的依赖。
// 缓存放在 root 所有的 .ferry 下，不交给 SFTP 上传用户修改。
func (a *Agent) uploadCacheDir(stage string) string {
	return filepath.Join(a.cacheRootDir(), filepath.Base(stage), "lib")
}

func (a *Agent) validUploadStage(stage string) bool {
	return filepath.Clean(stage) == stage && filepath.Dir(stage) == a.incomingDir() && reID.MatchString(filepath.Base(stage))
}

func (a *Agent) ensureUploadCache(stage string) error {
	if !a.validUploadStage(stage) {
		return fmt.Errorf("上传目录无效：%s", stage)
	}
	cache := a.uploadCacheDir(stage)
	for _, p := range []string{a.cacheRootDir(), filepath.Dir(cache), cache} {
		if err := os.Mkdir(p, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("上传缓存不是普通目录：%s", p)
		}
	}
	return nil
}

func (a *Agent) removeUpload(stage string) error {
	if !a.validUploadStage(stage) {
		return fmt.Errorf("上传目录无效：%s", stage)
	}
	for _, p := range []string{a.req.Root, a.meta(), a.incomingDir(), a.cacheRootDir()} {
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("拒绝清理非普通目录下的上传：%s", p)
		}
	}
	return errors.Join(os.RemoveAll(stage), os.RemoveAll(filepath.Dir(a.uploadCacheDir(stage))))
}

func (a *Agent) finishUpload(stage string) {
	if err := a.removeUpload(stage); err != nil {
		a.warn(proto.EvPrune, "", "本次上传残留未清理：%v（下次准备上传时重试）", err)
	}
}

// discardUpload 上传失败、取消、备份失败或其他目标失败而不再切换时，由引擎释放上传。
func (a *Agent) discardUpload() int {
	if err := a.removeUpload(a.req.Stage); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	return a.result(proto.CodeOK, "", nil)
}
