package agent

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/s3c"
)

// 异地备份（backups 的 upload）：定时、手动备份成功后，把这份备份加密、传到 S3 兼容的对象存储，
// 再删掉那边多出来的旧备份。服务器坏了、被删、被勒索加密时，在新服务器上「从异地取回」再恢复。
//
//	<prefix><名字>/<名字>-20261112-030000.sql.gz.age   加密的（age 格式，密码是 envFile 里的 FERRY_BACKUP_PASSWORD）
//	<prefix><名字>/<名字>-20261112-030000.sql.gz       没加密的（upload.encrypt: false）
//
// 部署前、恢复前的那份不传（不拖慢部署、恢复；本机有就够了）。传失败不算备份失败，记在这次的结果里，告警会提醒。

const (
	passwordVar = "FERRY_BACKUP_PASSWORD"
	ageSuffix   = ".age"
	// ageWorkFactor 加密密码的 scrypt 强度：2^16（约 64 MB 内存，零点几秒）；age 命令行解得开
	ageWorkFactor = 16
)

// uploadReasons 这几种备份做完要传到异地。
var uploadReasons = map[string]bool{"定时": true, "手动": true}

func envMap(it proto.BackupItem) (map[string]string, error) {
	list, err := backupEnv(it)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, kv := range list {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m, nil
}

// s3For 这一项的对象存储：访问密钥从 envFile 读（S3_ACCESS_KEY_ID / AWS_ACCESS_KEY_ID 都认）。
func s3For(it proto.BackupItem, env map[string]string) (*s3c.Client, error) {
	u := it.Upload
	ak, sk := env["S3_ACCESS_KEY_ID"], env["S3_SECRET_ACCESS_KEY"]
	if ak == "" {
		ak, sk = env["AWS_ACCESS_KEY_ID"], env["AWS_SECRET_ACCESS_KEY"]
	}
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("envFile（%s）里没有对象存储的访问密钥：写上 S3_ACCESS_KEY_ID=… 和 S3_SECRET_ACCESS_KEY=…", strings.Join(it.EnvFiles, "、"))
	}
	return s3c.New(u.Endpoint, u.Bucket, u.Region, ak, sk, u.PathStyle)
}

func remoteDir(it proto.BackupItem) string { return it.Upload.Prefix + it.Name + "/" }

// remoteBase 异地的文件名去掉 .age 后是不是这一项的备份文件。
func remoteBase(it proto.BackupItem, name string) (string, bool) {
	base := strings.TrimSuffix(name, ageSuffix)
	return base, reBackupFile.MatchString(base) && strings.HasPrefix(base, it.Name+"-")
}

// uploadBackup 把刚做好的这份传到异地；结果记在 r 里（Uploaded / UploadError）。
func (a *Agent) uploadBackup(root string, it proto.BackupItem, r *proto.BackupRun) {
	if it.Upload == nil || !r.OK {
		return
	}
	start := a.Sys.Now()
	where, err := a.upload(root, it, r.File)
	if err != nil {
		r.UploadError = err.Error()
		a.warn(proto.EvWarn, "", "%s 没传到异地：%v（服务器上的这份是好的）", r.File, err)
		return
	}
	r.Uploaded = where
	a.info(proto.EvInfo, "", "已传到异地：%s（%.1f 秒）", where, a.Sys.Now().Sub(start).Seconds())
}

func (a *Agent) upload(root string, it proto.BackupItem, file string) (string, error) {
	env, err := envMap(it)
	if err != nil {
		return "", err
	}
	c, err := s3For(it, env)
	if err != nil {
		return "", err
	}
	src := filepath.Join(backupItemDir(root, it.Name), file)
	key := remoteDir(it) + file
	if it.Upload.Encrypt {
		pass := env[passwordVar]
		if pass == "" {
			return "", fmt.Errorf("envFile（%s）里没有加密用的密码：写上 %s=…（记在别处一份，取回时要用；不想加密的话在 upload 里写 encrypt: false）", strings.Join(it.EnvFiles, "、"), passwordVar)
		}
		tmp := filepath.Join(backupItemDir(root, it.Name), "."+file+ageSuffix+".part")
		defer os.Remove(tmp)
		if err := encryptFile(src, tmp, pass); err != nil {
			return "", fmt.Errorf("加密失败：%v", err)
		}
		src, key = tmp, key+ageSuffix
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	a.info(proto.EvInfo, "", "传到异地 %s…", c.Where(key))
	if err := c.PutFile(ctx, key, src); err != nil {
		return "", err
	}
	if err := pruneRemote(ctx, c, it); err != nil {
		a.warn(proto.EvWarn, "", "删异地多出来的旧备份失败：%v", err)
	}
	return c.Where(key), nil
}

// pruneRemote 异地只留最近 keep 份。
func pruneRemote(ctx context.Context, c *s3c.Client, it proto.BackupItem) error {
	objs, err := c.List(ctx, remoteDir(it))
	if err != nil {
		return err
	}
	var mine []s3c.Object
	for _, o := range objs {
		if _, ok := remoteBase(it, path.Base(o.Key)); ok && path.Dir(o.Key)+"/" == remoteDir(it) {
			mine = append(mine, o)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return path.Base(mine[i].Key) > path.Base(mine[j].Key) })
	keep := max(it.Upload.Keep, 1)
	for i := keep; i < len(mine); i++ {
		if err := c.Delete(ctx, mine[i].Key); err != nil {
			return err
		}
	}
	return nil
}

func encryptFile(src, dst, pass string) error {
	rcp, err := age.NewScryptRecipient(pass)
	if err != nil {
		return err
	}
	rcp.SetWorkFactor(ageWorkFactor)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w, err := age.Encrypt(out, rcp)
	if err == nil {
		_, err = io.Copy(w, in)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	if serr := out.Sync(); err == nil {
		err = serr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---------------------------------------------------------------- 列出、取回

// remoteFiles 异地的备份（新的在前）；服务器上已经有同一份的标上 Local。
func (a *Agent) remoteFiles(root string, it proto.BackupItem) ([]proto.BackupFile, error) {
	if it.Upload == nil {
		return nil, fmt.Errorf("备份 %s 没有设 upload（传到异地）", it.Name)
	}
	env, err := envMap(it)
	if err != nil {
		return nil, err
	}
	c, err := s3For(it, env)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	objs, err := c.List(ctx, remoteDir(it))
	if err != nil {
		return nil, err
	}
	out := []proto.BackupFile{}
	for _, o := range objs {
		name := path.Base(o.Key)
		base, ok := remoteBase(it, name)
		if !ok || path.Dir(o.Key)+"/" != remoteDir(it) {
			continue
		}
		out = append(out, proto.BackupFile{Item: it.Name, Name: name, Path: c.Where(o.Key), Size: o.Size, Time: o.Modified, Remote: true,
			Local: exists(filepath.Join(backupItemDir(root, it.Name), base))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// fetchBackup 把异地的 file 下载回服务器的备份目录（加密的先解密），返回服务器上的文件名。
func (a *Agent) fetchBackup(root string, it proto.BackupItem, file string) (string, error) {
	if it.Upload == nil {
		return "", fmt.Errorf("备份 %s 没有设 upload（传到异地）", it.Name)
	}
	base, ok := remoteBase(it, file)
	if !ok || strings.ContainsAny(file, "/\\") {
		return "", fmt.Errorf("不是 %s 的备份文件：%s", it.Name, file)
	}
	dir := backupItemDir(root, it.Name)
	final := filepath.Join(dir, base)
	if exists(final) {
		return "", fmt.Errorf("服务器上已经有这一份了：%s", final)
	}
	env, err := envMap(it)
	if err != nil {
		return "", err
	}
	encrypted := strings.HasSuffix(file, ageSuffix)
	pass := env[passwordVar]
	if encrypted && pass == "" {
		return "", fmt.Errorf("这份是加密的：envFile（%s）里要有当初加密用的密码 %s=…", strings.Join(it.EnvFiles, "、"), passwordVar)
	}
	c, err := s3For(it, env)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	os.Chmod(backupDir(root), 0o700)
	dl := filepath.Join(dir, "."+file+".download")
	defer os.Remove(dl)
	f, err := os.OpenFile(dl, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	a.info(proto.EvInfo, "", "从异地下载 %s…", c.Where(remoteDir(it)+file))
	if _, err := c.Get(ctx, remoteDir(it)+file, f); err != nil {
		return "", err
	}
	part := filepath.Join(dir, "."+base+".part")
	defer os.Remove(part)
	if encrypted {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		if err := decryptTo(f, part, pass); err != nil {
			return "", err
		}
	} else {
		f.Close()
		if err := os.Rename(dl, part); err != nil {
			return "", err
		}
	}
	if err := checkGzip(part); err != nil {
		return "", fmt.Errorf("取回的文件不对（不是完整的 gzip）：%v", err)
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	a.info(proto.EvInfo, "", "已取回 %s：现在可以用它恢复", final)
	return base, nil
}

func decryptTo(r io.Reader, dst, pass string) error {
	id, err := age.NewScryptIdentity(pass)
	if err != nil {
		return err
	}
	dr, err := age.Decrypt(r, id)
	if err != nil {
		var wrong *age.NoIdentityMatchError
		if errors.As(err, &wrong) || strings.Contains(err.Error(), "incorrect passphrase") {
			return fmt.Errorf("解密失败：%s 不是当初加密用的密码", passwordVar)
		}
		return fmt.Errorf("解密失败：%v", err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, dr)
	if err != nil {
		err = fmt.Errorf("解密失败：%v", err)
	}
	if serr := out.Sync(); err == nil {
		err = serr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// checkGzip 整个读一遍，确认是完整的 gzip（校验和对得上）。
func checkGzip(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, zr)
	return err
}
