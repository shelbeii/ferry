package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 打开网页前试账号：对认得的控制台，经这台服务器调它的登录接口（和浏览器登录一样），看账号密码对不对。
// 只调登录接口、每个账号一次；不认得的软件（1Panel 有验证码、安全入口）不试。

// webLoginCheck 一种控制台怎么试登录：返回 (对, 能判断)；不能判断（接口变了、出错）时 known 为 false。
type webLoginCheck func(ctx context.Context, c *http.Client, base, path, user, pw string) (ok, known bool, err error)

var webLoginChecks = map[string]webLoginCheck{
	"nacos": func(ctx context.Context, c *http.Client, base, path, user, pw string) (bool, bool, error) {
		// 3.x 的控制台：/v3/auth/user/login（控制台的路径下）；2.x：/nacos/v1/auth/users/login
		ep := strings.TrimRight(path, "/") + "/v3/auth/user/login"
		if strings.Contains(path, "nacos") {
			ep = strings.TrimRight(path, "/") + "/v1/auth/users/login"
		}
		return statusCheck(postForm(ctx, c, base+ep, url.Values{"username": {user}, "password": {pw}}))
	},
	"minio": func(ctx context.Context, c *http.Client, base, _, user, pw string) (bool, bool, error) {
		return statusCheck(postJSON(ctx, c, base+"/api/v1/login", map[string]string{"accessKey": user, "secretKey": pw}))
	},
	"grafana": func(ctx context.Context, c *http.Client, base, path, user, pw string) (bool, bool, error) {
		return statusCheck(postJSON(ctx, c, base+strings.TrimRight(path, "/")+"/login", map[string]string{"user": user, "password": pw}))
	},
	"emqx": func(ctx context.Context, c *http.Client, base, _, user, pw string) (bool, bool, error) {
		return statusCheck(postJSON(ctx, c, base+"/api/v5/login", map[string]string{"username": user, "password": pw}))
	},
	"rabbitmq": func(ctx context.Context, c *http.Client, base, path, user, pw string) (bool, bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+strings.TrimRight(path, "/")+"/api/whoami", nil)
		if err != nil {
			return false, false, err
		}
		req.SetBasicAuth(user, pw)
		return statusCheck(c.Do(req))
	},
	"sentinel": func(ctx context.Context, c *http.Client, base, path, user, pw string) (bool, bool, error) {
		q := url.Values{"username": {user}, "password": {pw}}
		resp, err := postForm(ctx, c, base+strings.TrimRight(path, "/")+"/auth/login?"+q.Encode(), nil)
		return bodyFlag(resp, err, func(b []byte) (bool, bool) {
			var r struct {
				Success *bool `json:"success"`
			}
			if json.Unmarshal(b, &r) != nil || r.Success == nil {
				return false, false
			}
			return *r.Success, true
		})
	},
	"xxljob": func(ctx context.Context, c *http.Client, base, path, user, pw string) (bool, bool, error) {
		resp, err := postForm(ctx, c, base+strings.TrimRight(path, "/")+"/login", url.Values{"userName": {user}, "password": {pw}})
		return bodyFlag(resp, err, func(b []byte) (bool, bool) {
			var r struct {
				Code int `json:"code"`
			}
			if json.Unmarshal(b, &r) != nil || r.Code == 0 {
				return false, false
			}
			return r.Code == 200, true
		})
	},
}

func postForm(ctx context.Context, c *http.Client, u string, v url.Values) (*http.Response, error) {
	var body io.Reader
	if v != nil {
		body = strings.NewReader(v.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.Do(req)
}

func postJSON(ctx context.Context, c *http.Client, u string, v any) (*http.Response, error) {
	b, _ := json.Marshal(v)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.Do(req)
}

// statusCheck 2xx 对，401 / 403 不对，其他（404 接口不存在、5xx）判断不了。
func statusCheck(resp *http.Response, err error) (bool, bool, error) {
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, true, nil
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return false, true, nil
	}
	return false, false, nil
}

func bodyFlag(resp *http.Response, err error, judge func([]byte) (bool, bool)) (bool, bool, error) {
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ok, known := judge(b)
	return ok, known, nil
}

// checkWebAccounts 试每个账号，把结果写进 Check（ok | bad；判断不了的留空）。连不上网页时都留空。
func (e *Engine) checkWebAccounts(s *session, q WebQuery, list []WebAccount) {
	check := webLoginChecks[q.Kind]
	if check == nil || len(list) == 0 {
		return
	}
	scheme := q.Scheme
	if scheme != "https" {
		scheme = "http"
	}
	reach := newReach(q.Container, q.InnerPort, q.HostPort)
	tn := &Tunnel{s: s}
	for i := range list {
		a := &list[i]
		_, _ = reach.try(func(ep reachEP) (bool, error) {
			addr := net.JoinHostPort(firstNonEmptyStr(ep.host, "127.0.0.1"), strconv.Itoa(ep.port))
			if ep.container != "" {
				var err error
				if addr, err = tn.ContainerAddr(e.ctx(), ep.container, ep.port); err != nil {
					return false, err
				}
			}
			host := addr
			if q.VHost != "" {
				host = q.VHost
			}
			c := &http.Client{
				Timeout: 8 * time.Second,
				Transport: &http.Transport{
					DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return tn.Dial(ctx, addr) },
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: q.VHost}, // 经 SSH 连服务器本机
				},
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}
			defer c.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(e.ctx(), 10*time.Second)
			defer cancel()
			ok, known, err := check(ctx, c, scheme+"://"+host, q.Path, a.User, a.Password)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) || strings.Contains(err.Error(), "连不上") || strings.Contains(err.Error(), "connect") {
					return false, err // 连不上这个地址：换下一个
				}
				return false, nil
			}
			if known {
				a.Check = map[bool]string{true: "ok", false: "bad"}[ok]
			}
			return ok, nil
		})
	}
}
