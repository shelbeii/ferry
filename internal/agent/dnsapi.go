package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DNS 验证（ACME dns-01）：在域名的 DNS 里加一条 TXT 记录 _acme-challenge.<域名>，验证完删掉。
// 支持阿里云 DNS、腾讯云 DNSPod、Cloudflare；密钥只需要改 DNS 记录的权限。

// dnsProvider 一家 DNS 服务商。
type dnsProvider interface {
	// Present 加一条 TXT 记录（fqdn 不带末尾的点），返回删除时用的 ID
	Present(ctx context.Context, fqdn, value string) (string, error)
	CleanUp(ctx context.Context, fqdn, id string) error
}

// DNSProviders 支持的 DNS 服务商与它们要的密钥字段。
var DNSProviders = map[string][]string{
	"aliyun":     {"id", "secret"}, // AccessKey ID、AccessKey Secret（RAM 用户，权限 AliyunDNSFullAccess）
	"dnspod":     {"id", "secret"}, // 腾讯云 SecretId、SecretKey（权限 QcloudDNSPodFullAccess）
	"cloudflare": {"token"},        // API Token（权限 Zone.DNS Edit）
}

var dnsNames = map[string]string{"aliyun": "阿里云 DNS", "dnspod": "腾讯云 DNSPod", "cloudflare": "Cloudflare"}

// dnsEndpoints 各家 API 的地址（测试时换成本地的假服务）。
var dnsEndpoints = map[string]string{
	"aliyun":     "https://alidns.aliyuncs.com/",
	"dnspod":     "https://dnspod.tencentcloudapi.com/",
	"cloudflare": "https://api.cloudflare.com/client/v4",
}

func newDNSProvider(name string, cred map[string]string, client *http.Client) (dnsProvider, error) {
	fields, ok := DNSProviders[name]
	if !ok {
		return nil, fmt.Errorf("不支持的 DNS 服务商：%s（支持 aliyun、dnspod、cloudflare）", name)
	}
	for _, f := range fields {
		if strings.TrimSpace(cred[f]) == "" {
			return nil, fmt.Errorf("没有%s的密钥：在 Ferry 的「设置 → 部署 → HTTPS 证书」里填上（或用 ferry dns-key set %s）", dnsNames[name], name)
		}
	}
	ep := dnsEndpoints[name]
	switch name {
	case "aliyun":
		return &aliDNS{id: cred["id"], secret: cred["secret"], ep: ep, c: client}, nil
	case "dnspod":
		return &dnspod{id: cred["id"], secret: cred["secret"], ep: ep, c: client}, nil
	default:
		return &cloudflare{token: cred["token"], ep: ep, c: client}, nil
	}
}

// ---------------------------------------------------------------- 阿里云

// aliDNS 阿里云 DNS（RPC 风格的 API，签名 v1：HMAC-SHA1）。
type aliDNS struct {
	id, secret, ep string
	c              *http.Client
}

func aliEscape(s string) string {
	return strings.NewReplacer("+", "%20", "*", "%2A", "%7E", "~").Replace(url.QueryEscape(s))
}

// aliSign 签名后的查询串。
func aliSign(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, aliEscape(k)+"="+aliEscape(params[k]))
	}
	q := strings.Join(parts, "&")
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte("GET&" + aliEscape("/") + "&" + aliEscape(q)))
	return q + "&Signature=" + aliEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

func (p *aliDNS) call(ctx context.Context, action string, args map[string]string, out any) error {
	params := map[string]string{
		"Action": action, "Format": "JSON", "Version": "2015-01-09", "AccessKeyId": p.id,
		"SignatureMethod": "HMAC-SHA1", "SignatureVersion": "1.0", "SignatureNonce": randHex(16),
		"Timestamp": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	for k, v := range args {
		params[k] = v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ep+"?"+aliSign(params, p.secret), nil)
	if err != nil {
		return err
	}
	b, status, err := doJSON(p.c, req)
	if err != nil {
		return fmt.Errorf("阿里云 DNS %s：%v", action, err)
	}
	var e struct{ Code, Message string }
	if json.Unmarshal(b, &e) == nil && e.Code != "" {
		return fmt.Errorf("阿里云 DNS %s：%s（%s）", action, e.Message, e.Code)
	}
	if status/100 != 2 {
		return fmt.Errorf("阿里云 DNS %s：返回 %d", action, status)
	}
	return json.Unmarshal(b, out)
}

func (p *aliDNS) zone(ctx context.Context, fqdn string) (domain, rr string, err error) {
	var r struct{ DomainName, RR string }
	if err := p.call(ctx, "GetMainDomainName", map[string]string{"InputString": fqdn}, &r); err != nil {
		return "", "", err
	}
	if r.DomainName == "" {
		return "", "", fmt.Errorf("阿里云 DNS 里没有 %s 所在的域名", fqdn)
	}
	return r.DomainName, r.RR, nil
}

func (p *aliDNS) Present(ctx context.Context, fqdn, value string) (string, error) {
	domain, rr, err := p.zone(ctx, fqdn)
	if err != nil {
		return "", err
	}
	var r struct{ RecordId string }
	err = p.call(ctx, "AddDomainRecord", map[string]string{"DomainName": domain, "RR": rr, "Type": "TXT", "Value": value, "TTL": "600"}, &r)
	return r.RecordId, err
}

func (p *aliDNS) CleanUp(ctx context.Context, _, id string) error {
	var r struct{}
	return p.call(ctx, "DeleteDomainRecord", map[string]string{"RecordId": id}, &r)
}

// ---------------------------------------------------------------- 腾讯云 DNSPod

// dnspod 腾讯云 API 3.0（签名 TC3-HMAC-SHA256）。
type dnspod struct {
	id, secret, ep string
	c              *http.Client
}

func hmac256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// tc3Canon 规范请求（签名的内容之一）：签 content-type、host、x-tc-action 三个头。payloadHash 是请求体的 sha256（十六进制）。
func tc3Canon(host, action, payloadHash string) string {
	return "POST\n/\n\ncontent-type:application/json; charset=utf-8\nhost:" + host + "\nx-tc-action:" + strings.ToLower(action) +
		"\n\ncontent-type;host;x-tc-action\n" + payloadHash
}

// tc3Auth Authorization 头。host 是请求的主机名，ts 是 Unix 秒。
func tc3Auth(id, secret, host, service, action string, ts int64, body []byte) string {
	date := time.Unix(ts, 0).UTC().Format("2006-01-02")
	scope := date + "/" + service + "/tc3_request"
	toSign := "TC3-HMAC-SHA256\n" + strconv.FormatInt(ts, 10) + "\n" + scope + "\n" + sha256hex([]byte(tc3Canon(host, action, sha256hex(body))))
	k := hmac256(hmac256(hmac256([]byte("TC3"+secret), date), service), "tc3_request")
	return "TC3-HMAC-SHA256 Credential=" + id + "/" + scope + ", SignedHeaders=content-type;host;x-tc-action, Signature=" + hex.EncodeToString(hmac256(k, toSign))
}

func (p *dnspod) call(ctx context.Context, action string, args any, out any) error {
	body, _ := json.Marshal(args)
	u, err := url.Parse(p.ep)
	if err != nil {
		return err
	}
	ts := time.Now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.ep, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", "2021-03-23")
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("Authorization", tc3Auth(p.id, p.secret, u.Host, "dnspod", action, ts, body))
	b, _, err := doJSON(p.c, req)
	if err != nil {
		return fmt.Errorf("DNSPod %s：%v", action, err)
	}
	var r struct {
		Response struct {
			Error *struct{ Code, Message string }
		}
	}
	if json.Unmarshal(b, &r) == nil && r.Response.Error != nil {
		return fmt.Errorf("DNSPod %s：%s（%s）", action, r.Response.Error.Message, r.Response.Error.Code)
	}
	return json.Unmarshal(b, out)
}

// zone 账号里的域名中，fqdn 属于哪一个（最长的后缀）。
func (p *dnspod) zone(ctx context.Context, fqdn string) (domain, sub string, err error) {
	var r struct {
		Response struct{ DomainList []struct{ Name string } }
	}
	if err := p.call(ctx, "DescribeDomainList", map[string]any{"Limit": 3000}, &r); err != nil {
		return "", "", err
	}
	for _, d := range r.Response.DomainList {
		n := strings.ToLower(d.Name)
		if strings.HasSuffix(fqdn, "."+n) && len(n) > len(domain) {
			domain = n
		}
	}
	if domain == "" {
		return "", "", fmt.Errorf("DNSPod 账号里没有 %s 所在的域名", fqdn)
	}
	return domain, strings.TrimSuffix(fqdn, "."+domain), nil
}

func (p *dnspod) Present(ctx context.Context, fqdn, value string) (string, error) {
	domain, sub, err := p.zone(ctx, fqdn)
	if err != nil {
		return "", err
	}
	var r struct {
		Response struct{ RecordId uint64 }
	}
	err = p.call(ctx, "CreateRecord", map[string]any{"Domain": domain, "SubDomain": sub, "RecordType": "TXT", "RecordLine": "默认", "Value": value, "TTL": 600}, &r)
	return domain + "/" + strconv.FormatUint(r.Response.RecordId, 10), err
}

func (p *dnspod) CleanUp(ctx context.Context, _, id string) error {
	domain, rid, ok := strings.Cut(id, "/")
	n, err := strconv.ParseUint(rid, 10, 64)
	if !ok || err != nil {
		return fmt.Errorf("记录 ID 无效：%s", id)
	}
	var r struct{}
	return p.call(ctx, "DeleteRecord", map[string]any{"Domain": domain, "RecordId": n}, &r)
}

// ---------------------------------------------------------------- Cloudflare

type cloudflare struct {
	token, ep string
	c         *http.Client
}

func (p *cloudflare) call(ctx context.Context, method, path string, args any, out any) error {
	var body io.Reader
	if args != nil {
		b, _ := json.Marshal(args)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.ep+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	b, _, err := doJSON(p.c, req)
	if err != nil {
		return fmt.Errorf("Cloudflare：%v", err)
	}
	var r struct {
		Success bool
		Errors  []struct {
			Code    int
			Message string
		}
		Result json.RawMessage
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("Cloudflare：返回的不是 JSON")
	}
	if !r.Success {
		if len(r.Errors) > 0 {
			return fmt.Errorf("Cloudflare：%s（%d）", r.Errors[0].Message, r.Errors[0].Code)
		}
		return errors.New("Cloudflare：请求失败")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

// zone 从长到短试 fqdn 的后缀，找到账号里的 zone。
func (p *cloudflare) zone(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(fqdn, ".")
	for i := 0; i < len(labels)-1; i++ {
		var zs []struct{ ID string }
		if err := p.call(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(strings.Join(labels[i:], ".")), nil, &zs); err != nil {
			return "", err
		}
		if len(zs) > 0 {
			return zs[0].ID, nil
		}
	}
	return "", fmt.Errorf("Cloudflare 账号里没有 %s 所在的域名", fqdn)
}

func (p *cloudflare) Present(ctx context.Context, fqdn, value string) (string, error) {
	z, err := p.zone(ctx, fqdn)
	if err != nil {
		return "", err
	}
	var r struct{ ID string }
	err = p.call(ctx, http.MethodPost, "/zones/"+z+"/dns_records", map[string]any{"type": "TXT", "name": fqdn, "content": value, "ttl": 120}, &r)
	return z + "/" + r.ID, err
}

func (p *cloudflare) CleanUp(ctx context.Context, _, id string) error {
	z, rid, ok := strings.Cut(id, "/")
	if !ok {
		return fmt.Errorf("记录 ID 无效：%s", id)
	}
	return p.call(ctx, http.MethodDelete, "/zones/"+z+"/dns_records/"+rid, nil, nil)
}

// doJSON 发请求、读回应（最多 1 MB）。错误里不带地址（阿里云的签名、令牌在地址里）。
func doJSON(c *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, err
}
