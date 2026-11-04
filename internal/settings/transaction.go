package settings

import (
	"errors"
	"strings"

	"github.com/captain/ferry/internal/secret"
)

// SecretChange 桌面设置草稿中的凭据变更，值仅经本次请求传递。
type SecretChange struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Value    string `json:"value,omitempty"`
	AccessID string `json:"accessId,omitempty"`
	Remove   bool   `json:"remove,omitempty"`
}

type accountValue struct{ name, value string }

// SaveWithSecrets 串行保存整批设置及凭据；任何可报告的提交失败都尝试恢复旧凭据。
// 返回已规范化的设置，调用方无需在提交后额外读取才能确认成功。
func SaveWithSecrets(s Settings, changes []SecretChange) (Settings, error) {
	saveMu.Lock()
	defer saveMu.Unlock()
	flat, err := secretChanges(changes)
	if err != nil {
		return Settings{}, err
	}
	s.normalize()
	commit := func() error { return save(s) }
	if len(flat) == 0 {
		err = commit()
	} else {
		err = secret.WithChanges(flat, commit)
	}
	if err != nil {
		return Settings{}, err
	}
	return s, nil
}

func secretChanges(changes []SecretChange) ([]secret.Change, error) {
	if len(changes) > 1024 {
		return nil, errors.New("本次凭据变更过多，未保存")
	}
	out := []secret.Change{}
	seen := map[string]bool{}
	add := func(account, value string, remove bool) error {
		if seen[account] {
			return errors.New("同一项凭据有重复变更，未保存")
		}
		seen[account] = true
		out = append(out, secret.Change{Account: account, Value: value, Delete: remove})
		return nil
	}
	for _, c := range changes {
		if c.ID != strings.TrimSpace(c.ID) || !reModelID.MatchString(c.ID) {
			return nil, errors.New("凭据所属的模型、通知或 DNS 标识无效，未保存")
		}
		value := strings.TrimSpace(c.Value)
		if c.Remove && (c.Value != "" || c.AccessID != "") {
			return nil, errors.New("删除凭据时不能同时提供新值，未保存")
		}
		if !c.Remove && value == "" {
			return nil, errors.New("不能保存空凭据，未保存")
		}
		var accounts []accountValue
		switch c.Kind {
		case "ai":
			accounts = append(accounts, accountValue{secret.AIModelKeyAccount(c.ID), value})
			if c.ID == LegacyModelID {
				if err := add(secret.AIKeyAccount, "", true); err != nil {
					return nil, err
				}
			}
		case "notify-url":
			if !c.Remove && !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
				return nil, errors.New("通知地址要以 https:// 或 http:// 开头，未保存")
			}
			accounts = append(accounts, accountValue{secret.NotifyURLAccount(c.ID), value})
		case "notify-sign":
			accounts = append(accounts, accountValue{secret.NotifySignAccount(c.ID), value})
		case "dns":
			switch c.ID {
			case "cloudflare":
				accounts = append(accounts, accountValue{secret.DNSAccount(c.ID, "token"), value})
			case "aliyun", "dnspod":
				id := strings.TrimSpace(c.AccessID)
				if !c.Remove && id == "" {
					return nil, errors.New("DNS 的访问 ID 和密钥必须一起填写，未保存")
				}
				accounts = append(accounts, accountValue{secret.DNSAccount(c.ID, "id"), id}, accountValue{secret.DNSAccount(c.ID, "secret"), value})
			default:
				return nil, errors.New("不支持的 DNS 服务商，未保存")
			}
		default:
			return nil, errors.New("不支持的设置凭据类型，未保存")
		}
		if c.Kind != "dns" && c.AccessID != "" {
			return nil, errors.New("这类凭据不接受访问 ID，未保存")
		}
		for _, a := range accounts {
			v := a.value
			if c.Remove {
				v = ""
			}
			if err := add(a.name, v, c.Remove); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
