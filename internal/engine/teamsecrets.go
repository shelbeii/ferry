package engine

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/flock"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/secret"
	"gopkg.in/yaml.v3"
)

const (
	ageIdentityEnv    = "FERRY_AGE_IDENTITY"
	maxTeamSecrets    = 1 << 20
	maxTeamRecipients = 128
)

var (
	errAgeIdentityMissing = errors.New("请在「团队共享」中生成本机公钥，或通过 FERRY_AGE_IDENTITY 提供 age 私钥")
	errTeamMissing        = errors.New("团队密钥文件尚未同步或初始化")
	errTeamFormat         = errors.New("团队密钥文件格式无效")
)

// teamSecretFile 只在内存中解密。保留所有应用的值和接收方，编辑一项时原样带回。
type teamSecretFile struct {
	Version    int                              `yaml:"version"`
	Recipients []string                         `yaml:"recipients"`
	Apps       map[string]proto.AppSecretValues `yaml:"apps"`
}

// TeamSecretsStatus 只返回本机公钥和已经解密验证过的接收方列表。
type TeamSecretsStatus struct {
	Path       string   `json:"path,omitempty"`
	PublicKey  string   `json:"publicKey,omitempty"`
	Recipients []string `json:"recipients,omitempty"`
	Exists     bool     `json:"exists"`
	Ready      bool     `json:"ready"`
	Problem    string   `json:"problem,omitempty"`
}

func localAgeIdentity() (*age.X25519Identity, error) {
	value, found, err := secret.Get(secret.AppAgeIdentityAccount)
	if err != nil {
		return nil, errors.New("无法读取本机的 age 凭据，请检查系统凭据库")
	}
	if !found {
		return nil, errAgeIdentityMissing
	}
	return parseAgeIdentity(value)
}

func parseAgeIdentity(value string) (*age.X25519Identity, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(value))
	if err != nil {
		return nil, errors.New("age 私钥格式无效，需要单行的 X25519 私钥")
	}
	return id, nil
}

func (e *Engine) teamReadIdentity() (*age.X25519Identity, error) {
	if value := e.getenv(ageIdentityEnv); value != "" {
		return parseAgeIdentity(value)
	}
	return localAgeIdentity()
}

// EnsureAppAgeIdentity 生成一次本机身份；重复调用只返回同一个公钥。
func EnsureAppAgeIdentity() (string, error) {
	unlock, err := flock.Lock(filepath.Join(appdir.LocksDir(), "app-age-identity.lock"), 5*time.Second, nil)
	if err != nil {
		return "", err
	}
	defer unlock()
	id, err := localAgeIdentity()
	if errors.Is(err, errAgeIdentityMissing) {
		id, err = age.GenerateX25519Identity()
		if err == nil {
			if err := secret.Set(secret.AppAgeIdentityAccount, id.String()); err != nil {
				return "", errors.New("无法保存本机的 age 私钥，请检查系统凭据库")
			}
		}
	}
	if err != nil {
		return "", err
	}
	return id.Recipient().String(), nil
}

func parseTeamRecipient(value string) (*age.X25519Recipient, error) {
	r, err := age.ParseX25519Recipient(strings.TrimSpace(value))
	if err != nil {
		return nil, errors.New("需要以 age1 开头的 X25519 公钥")
	}
	return r, nil
}

func (f *teamSecretFile) checkedRecipients() ([]age.Recipient, error) {
	if f.Version != 1 || len(f.Recipients) == 0 || len(f.Recipients) > maxTeamRecipients {
		return nil, errTeamFormat
	}
	seen := map[string]bool{}
	out := make([]age.Recipient, 0, len(f.Recipients))
	for _, key := range f.Recipients {
		r, err := parseTeamRecipient(key)
		if err != nil || key != r.String() || seen[key] {
			return nil, errTeamFormat
		}
		seen[key] = true
		out = append(out, r)
	}
	return out, nil
}

func readTeamSecretFile(root *os.Root, file string, id *age.X25519Identity) (*teamSecretFile, error) {
	info, err := root.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errTeamMissing
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2*maxTeamSecrets {
		return nil, errors.New("无法读取项目目录内的团队密钥文件")
	}
	src, err := root.Open(file)
	if err != nil {
		return nil, errors.New("无法打开团队密钥文件")
	}
	defer src.Close()
	r, err := age.Decrypt(io.LimitReader(src, 2*maxTeamSecrets), id)
	var unmatched *age.NoIdentityMatchError
	switch {
	case errors.As(err, &unmatched):
		return nil, errors.New("当前 age 公钥不在收件人中，请让一名已有成员共享文件")
	case err != nil:
		return nil, errors.New("团队密钥文件无法解密，请检查文件完整性")
	}
	cleartext, err := io.ReadAll(io.LimitReader(r, maxTeamSecrets+1))
	if err != nil || len(cleartext) > maxTeamSecrets {
		return nil, errors.New("团队密钥文件损坏或过大")
	}
	defer clear(cleartext)
	var f teamSecretFile
	d := yaml.NewDecoder(bytes.NewReader(cleartext))
	d.KnownFields(true)
	// YAML 或 age 的错误可能包含原始内容，统一返回与内容无关的错误。
	if d.Decode(&f) != nil || !errors.Is(d.Decode(new(any)), io.EOF) {
		return nil, errTeamFormat
	}
	if _, err := f.checkedRecipients(); err != nil || !slices.Contains(f.Recipients, id.Recipient().String()) {
		return nil, errTeamFormat
	}
	if f.Apps == nil {
		f.Apps = make(map[string]proto.AppSecretValues)
	}
	return &f, nil
}

func writeTeamSecretFile(root *os.Root, file string, f *teamSecretFile, create bool) error {
	recipients, err := f.checkedRecipients()
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return errTeamFormat
	}
	defer clear(data)
	if len(data) > maxTeamSecrets {
		return errors.New("团队密钥文件过大")
	}
	tmp := "." + path.Base(file) + "." + rand.Text() + ".tmp"
	out, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	defer out.Close()
	w, err := age.Encrypt(out, recipients...)
	if err != nil {
		return errors.New("无法加密团队密钥，请检查收件人公钥")
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if create {
		// 初始化不能覆盖已有文件，即使编辑期间别的进程把文件同步进来。
		return root.Link(tmp, file)
	}
	return root.Rename(tmp, file)
}

func (e *Engine) readTeamSecrets(t *config.Target, id *age.X25519Identity) (*teamSecretFile, error) {
	root, err := os.OpenRoot(e.File.Dir)
	if err != nil {
		return nil, fmt.Errorf("打开项目配置目录失败：%w", err)
	}
	defer root.Close()
	return readTeamSecretFile(root, t.AppSecretsFile(), id)
}

func (e *Engine) TeamSecrets(target string) (TeamSecretsStatus, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return TeamSecretsStatus{}, err
	}
	status := TeamSecretsStatus{Path: t.AppSecretsFile()}
	if status.Path == "" {
		return status, nil
	}
	root, err := os.OpenRoot(e.File.Dir)
	if err != nil {
		return status, err
	}
	defer root.Close()
	_, statErr := root.Lstat(status.Path)
	status.Exists = !errors.Is(statErr, os.ErrNotExist)
	id, err := e.teamReadIdentity()
	if err != nil {
		status.Problem = err.Error()
		return status, nil
	}
	status.PublicKey = id.Recipient().String()
	f, err := readTeamSecretFile(root, status.Path, id)
	if err != nil {
		status.Problem = err.Error()
		return status, nil
	}
	status.Ready, status.Recipients = true, f.Recipients
	return status, nil
}

// editTeamSecrets 在锁内重新解密，避免两个 Ferry 进程覆盖对方保存的值。
func (e *Engine) editTeamSecrets(t *config.Target, init bool, edit func(*teamSecretFile, string) (bool, error)) error {
	file := t.AppSecretsFile()
	if file == "" {
		return errors.New("这个目标使用本机凭据库；请先配置 secretsFrom: file:secrets/<目标>.age")
	}
	id, err := e.teamReadIdentity()
	if err != nil {
		return err
	}
	base, err := filepath.EvalSymlinks(e.File.Dir)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := path.Dir(file)
	if init {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	folder, err := root.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return errTeamMissing
	}
	if err != nil {
		return err
	}
	defer folder.Close()
	file = path.Base(file)
	lockPath, err := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(dir)))
	if err != nil {
		return err
	}
	lockName := filepath.Join(lockPath, file)
	// 大小写不敏感的文件系统上，不同拼写的路径仍需共用同一把锁。
	key := sha256.Sum256([]byte(strings.ToLower(lockName)))
	unlock, err := flock.Lock(filepath.Join(appdir.LocksDir(), fmt.Sprintf("team-secrets-%x.lock", key)), 5*time.Second, nil)
	if err != nil {
		return err
	}
	defer unlock()

	pub := id.Recipient().String()
	if init {
		f := &teamSecretFile{Version: 1, Recipients: []string{pub}, Apps: map[string]proto.AppSecretValues{}}
		if err := writeTeamSecretFile(folder, file, f, true); errors.Is(err, os.ErrExist) {
			return errors.New("文件已存在，请同步后打开团队共享")
		} else {
			return err
		}
	}
	f, err := readTeamSecretFile(folder, file, id)
	if err != nil {
		return err
	}
	changed, err := edit(f, pub)
	if err != nil || !changed {
		return err
	}
	return writeTeamSecretFile(folder, file, f, false)
}

func (e *Engine) InitTeamSecrets(target string) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	return e.editTeamSecrets(t, true, nil)
}

func (e *Engine) EditTeamRecipient(target, key string, add bool) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	r, err := parseTeamRecipient(key)
	if err != nil {
		return err
	}
	key = r.String()
	return e.editTeamSecrets(t, false, func(f *teamSecretFile, pub string) (bool, error) {
		index := slices.Index(f.Recipients, key)
		switch {
		case add && index >= 0, !add && index < 0:
			return false, nil
		case add:
			if len(f.Recipients) >= maxTeamRecipients {
				return false, fmt.Errorf("每个团队密钥文件最多共享给 %d 个公钥", maxTeamRecipients)
			}
			f.Recipients = append(f.Recipients, key)
		case key == pub:
			return false, errors.New("请让另一位已有访问权限的成员移除本机公钥")
		default:
			f.Recipients = slices.Delete(f.Recipients, index, index+1)
		}
		return true, nil
	})
}

func (e *Engine) saveTeamSecret(t *config.Target, app, name, value string) error {
	return e.editTeamSecrets(t, false, func(f *teamSecretFile, _ string) (bool, error) {
		values := f.Apps[app]
		if values[name] == value {
			return false, nil
		}
		if value == "" {
			delete(values, name)
			if len(values) == 0 {
				delete(f.Apps, app)
			}
		} else {
			if values == nil {
				values = proto.AppSecretValues{}
				f.Apps[app] = values
			}
			values[name] = value
		}
		return true, nil
	})
}
