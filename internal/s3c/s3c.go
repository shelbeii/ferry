// Package s3c 一个够用的 S3 兼容对象存储客户端（异地备份用）：上传文件（大文件分片）、列出、删除、下载。
// 用 AWS 签名 V4，阿里云 OSS、腾讯云 COS、七牛、MinIO、AWS S3 等兼容 S3 接口的都能用。
// 没用官方 SDK：只要这几个操作，自己写几百行，agent 不用多带几 MB。
package s3c

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client 一个桶。
type Client struct {
	Scheme, Host string // https、oss-cn-hangzhou.aliyuncs.com（可以带端口）
	Bucket       string
	Region       string
	AccessKey    string
	SecretKey    string
	PathStyle    bool // http://host/bucket/key；false = http://bucket.host/key
	HTTP         *http.Client
	Now          func() time.Time
	// PartSize 分片上传每片多大；MultipartAbove 文件大于它才分片（测试时改小）
	PartSize, MultipartAbove int64
}

// Object 桶里的一个文件。
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

var reHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:\d+)?$`)

// New endpoint 是 https://oss-cn-hangzhou.aliyuncs.com 这样的地址；region 空时按地址猜。
// pathStyle：地址是 IP、带端口（自己搭的 MinIO）时总是用路径形式。
func New(endpoint, bucket, region, accessKey, secretKey string, pathStyle bool) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !reHost.MatchString(u.Host) || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("endpoint 要写成 https://域名 或 http://地址:端口（当前 %q）", endpoint)
	}
	if !ValidBucket(bucket) {
		return nil, fmt.Errorf("bucket 名字不对：%q", bucket)
	}
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("没有访问密钥")
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil || u.Port() != "" || strings.Contains(bucket, ".") {
		pathStyle = true
	}
	if region == "" {
		region = GuessRegion(host)
	}
	return &Client{Scheme: u.Scheme, Host: u.Host, Bucket: bucket, Region: region, AccessKey: accessKey, SecretKey: secretKey,
		PathStyle: pathStyle, HTTP: httpClient, Now: time.Now, PartSize: 64 << 20, MultipartAbove: 512 << 20}, nil
}

// httpClient 不设总超时（大文件要传很久，由调用方的 ctx 控制）；连接、等响应头各有超时，走服务器的代理设置。
var httpClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   30 * time.Second,
	ResponseHeaderTimeout: 2 * time.Minute,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       90 * time.Second,
}}

var reBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// ValidBucket 桶名：3～63 个小写字母、数字、点、横线。
func ValidBucket(b string) bool { return reBucket.MatchString(b) }

var (
	reAWS = regexp.MustCompile(`^s3[.-]([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)
	reOSS = regexp.MustCompile(`^(oss-[a-z0-9-]+?)(-internal)?\.aliyuncs\.com$`)
	reCOS = regexp.MustCompile(`^cos\.([a-z0-9-]+)\.myqcloud\.com$`)
)

// GuessRegion 按地址猜签名用的区域：AWS（s3.us-west-2.amazonaws.com）、阿里云 OSS（oss-cn-hangzhou）、
// 腾讯云 COS（ap-guangzhou）；猜不到用 us-east-1（MinIO 默认）。
func GuessRegion(host string) string {
	host = strings.ToLower(host)
	if m := reAWS.FindStringSubmatch(host); m != nil && m[1] != "external-1" {
		return m[1]
	}
	if m := reOSS.FindStringSubmatch(host); m != nil {
		return m[1]
	}
	if m := reCOS.FindStringSubmatch(host); m != nil {
		return m[1]
	}
	return "us-east-1"
}

// Where 给人看的位置：s3://桶/键
func (c *Client) Where(key string) string { return "s3://" + c.Bucket + "/" + key }

// ---------------------------------------------------------------- 签名、请求

// escapePath URI 编码，保留 /（S3 的键不再二次编码）。
func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		ch := p[i]
		if ch == '/' || unreserved(ch) {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func unreserved(ch byte) bool {
	return ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '~'
}

func escapeQuery(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if unreserved(s[i]) {
			b.WriteByte(s[i])
		} else {
			fmt.Fprintf(&b, "%%%02X", s[i])
		}
	}
	return b.String()
}

// canonicalQuery 按键排序、编码后的查询串（签名和请求用同一个）。
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, escapeQuery(k)+"="+escapeQuery(v))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// request 一个签好名的请求。body 可以为 nil；payloadHash 是 body 的 SHA-256（十六进制）。
func (c *Client) request(ctx context.Context, method, key string, q url.Values, body io.Reader, size int64, payloadHash string) (*http.Request, error) {
	host, path := c.Host, "/"+key
	if c.PathStyle {
		path = "/" + c.Bucket + "/" + key
		if key == "" {
			path = "/" + c.Bucket
		}
	} else {
		host = c.Bucket + "." + c.Host
	}
	epath := escapePath(path)
	query := canonicalQuery(q)
	u := c.Scheme + "://" + host + epath
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	if payloadHash == "" {
		payloadHash = emptySHA
	}
	now := c.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Host = host
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{method, epath, query,
		"host:" + host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n",
		signed, payloadHash}, "\n")
	scope := day + "/" + c.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	k := hmacSHA([]byte("AWS4"+c.SecretKey), day)
	k = hmacSHA(k, c.Region)
	k = hmacSHA(k, "s3")
	k = hmacSHA(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
	return req, nil
}

// s3Error 对象存储返回的错误。
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e s3Error
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		msg := e.Code
		if e.Message != "" {
			msg += "：" + e.Message
		}
		switch e.Code {
		case "SignatureDoesNotMatch":
			msg += "（密钥不对，或 region 不对：在 upload.s3.region 里写上桶所在的区域）"
		case "InvalidAccessKeyId":
			msg += "（访问密钥 ID 不对）"
		case "NoSuchBucket":
			msg += "（桶不存在：先在对象存储的控制台建好）"
		case "AccessDenied":
			msg += "（这个密钥没有这个桶的权限）"
		}
		return fmt.Errorf("对象存储返回 %d %s", resp.StatusCode, msg)
	}
	return fmt.Errorf("对象存储返回 %d %s", resp.StatusCode, strings.TrimSpace(string(firstLine(b))))
}

func firstLine(b []byte) []byte {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return b
}

// do 发请求（网络出错、5xx 时最多再试 2 次；body 每次重新取）。
func (c *Client) do(ctx context.Context, method, key string, q url.Values, body func() io.Reader, size int64, payloadHash string) (*http.Response, error) {
	var last error
	for i := 0; i < 3; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i*i) * 2 * time.Second):
			}
		}
		var r io.Reader
		if body != nil {
			r = body()
		}
		req, err := c.request(ctx, method, key, q, r, size, payloadHash)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			last = fmt.Errorf("连不上对象存储：%v", err)
			if ctx.Err() != nil {
				return nil, last
			}
			continue
		}
		if resp.StatusCode >= 500 {
			last = readError(resp)
			resp.Body.Close()
			continue
		}
		if resp.StatusCode >= 300 {
			err := readError(resp)
			resp.Body.Close()
			return nil, err
		}
		return resp, nil
	}
	return nil, last
}

// ---------------------------------------------------------------- 操作

// fileHash 文件 [off, off+n) 的 SHA-256。
func fileHash(f *os.File, off, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, off, n)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PutFile 上传本机文件；大于 MultipartAbove 时分片上传。
func (c *Client) PutFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size > c.MultipartAbove {
		return c.putMultipart(ctx, key, f, size)
	}
	hash, err := fileHash(f, 0, size)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, key, nil, func() io.Reader { return io.NewSectionReader(f, 0, size) }, size, hash)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (c *Client) putMultipart(ctx context.Context, key string, f *os.File, size int64) error {
	part := c.PartSize
	if size/part >= 9000 { // 最多 10000 片
		part = size/9000 + 1
	}
	resp, err := c.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, 0, "")
	if err != nil {
		return err
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&init)
	resp.Body.Close()
	if err != nil || init.UploadID == "" {
		return fmt.Errorf("开始分片上传失败：%v", err)
	}
	abort := func() {
		actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if r, err := c.do(actx, http.MethodDelete, key, url.Values{"uploadId": {init.UploadID}}, nil, 0, ""); err == nil {
			r.Body.Close()
		}
	}
	type done struct {
		XMLName xml.Name `xml:"Part"`
		N       int      `xml:"PartNumber"`
		ETag    string   `xml:"ETag"`
	}
	var parts []done
	for n, off := 1, int64(0); off < size; n, off = n+1, off+part {
		ln := min(part, size-off)
		hash, err := fileHash(f, off, ln)
		if err != nil {
			abort()
			return err
		}
		q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {init.UploadID}}
		o := off
		resp, err := c.do(ctx, http.MethodPut, key, q, func() io.Reader { return io.NewSectionReader(f, o, ln) }, ln, hash)
		if err != nil {
			abort()
			return fmt.Errorf("上传第 %d 片失败：%v", n, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		parts = append(parts, done{N: n, ETag: resp.Header.Get("ETag")})
	}
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []done
	}{Parts: parts})
	resp, err = c.do(ctx, http.MethodPost, key, url.Values{"uploadId": {init.UploadID}}, func() io.Reader { return bytes.NewReader(body) }, int64(len(body)), sha256Hex(body))
	if err != nil {
		abort()
		return err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var e s3Error
	if bytes.Contains(b, []byte("<Error>")) && xml.Unmarshal(b, &e) == nil && e.Code != "" { // 200 里也可能是错误
		abort()
		return fmt.Errorf("合并分片失败：%s %s", e.Code, e.Message)
	}
	return nil
}

// List 列出键以 prefix 开头的文件。
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for page := 0; page < 1000; page++ {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, 0, "")
		if err != nil {
			return nil, err
		}
		var r struct {
			Contents []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				LastModified string `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&r)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("看不懂对象存储返回的列表：%v", err)
		}
		for _, o := range r.Contents {
			t, _ := time.Parse(time.RFC3339, o.LastModified)
			out = append(out, Object{Key: o.Key, Size: o.Size, Modified: t})
		}
		if !r.IsTruncated || r.Next == "" {
			break
		}
		token = r.Next
	}
	return out, nil
}

// Delete 删掉一个文件。
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, 0, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Get 下载一个文件写到 w，返回写了多少。
func (c *Client) Get(ctx context.Context, key string, w io.Writer) (int64, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, 0, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, resp.Body)
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = fmt.Errorf("没下载完（%d / %d 字节）", n, resp.ContentLength)
	}
	return n, err
}
