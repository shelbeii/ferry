package s3c

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// AWS 文档里签名 V4 的例子（Signature Calculations for the Authorization Header: Transferring Payload in a Single Chunk）。
func TestSignatureAWSExamples(t *testing.T) {
	c := &Client{Scheme: "https", Host: "s3.amazonaws.com", Bucket: "examplebucket", Region: "us-east-1",
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }}
	for _, tc := range []struct {
		q    map[string][]string
		want string
	}{
		{map[string][]string{"lifecycle": {""}}, "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"},
		{map[string][]string{"max-keys": {"2"}, "prefix": {"J"}}, "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	} {
		req, err := c.request(context.Background(), http.MethodGet, "", tc.q, nil, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+tc.want) {
			t.Errorf("%v：%s", tc.q, got)
		}
		if req.URL.Host != "examplebucket.s3.amazonaws.com" {
			t.Error(req.URL)
		}
	}
}

func TestNewAndRegion(t *testing.T) {
	for host, want := range map[string]string{
		"s3.us-west-2.amazonaws.com":            "us-west-2",
		"s3-ap-southeast-1.amazonaws.com":       "ap-southeast-1",
		"s3.cn-north-1.amazonaws.com.cn":        "cn-north-1",
		"oss-cn-hangzhou.aliyuncs.com":          "oss-cn-hangzhou",
		"oss-cn-shanghai-internal.aliyuncs.com": "oss-cn-shanghai",
		"cos.ap-guangzhou.myqcloud.com":         "ap-guangzhou",
		"minio.example.com":                     "us-east-1",
	} {
		if got := GuessRegion(host); got != want {
			t.Errorf("%s：%s", host, got)
		}
	}
	c, err := New("http://10.0.0.5:9000", "backups", "", "ak", "sk", false)
	if err != nil || !c.PathStyle || c.Region != "us-east-1" {
		t.Fatalf("IP 加端口用路径形式：%+v %v", c, err)
	}
	c, err = New("https://oss-cn-hangzhou.aliyuncs.com/", "my-backups", "", "ak", "sk", false)
	if err != nil || c.PathStyle || c.Host != "oss-cn-hangzhou.aliyuncs.com" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range [][2]string{{"oss-cn-hangzhou.aliyuncs.com", "b1"}, {"ftp://x", "b1x"}, {"https://x/path", "b1x"}, {"https://x", "B"}} {
		if _, err := New(bad[0], bad[1], "", "ak", "sk", false); err == nil {
			t.Errorf("应拒绝 %v", bad)
		}
	}
	if _, err := New("https://x.com", "abc", "", "", "", false); err == nil {
		t.Error("没有密钥应拒绝")
	}
}

// fakeS3 内存里的桶：检查请求带了签名，支持 PUT / 分片 / 列出 / 删除 / 下载。
type fakeS3 struct {
	mu      sync.Mutex
	objs    map[string][]byte
	uploads map[string]map[int][]byte
	fail500 int // 前几次请求返回 500
	reqs    []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ak/") || r.Header.Get("x-amz-content-sha256") == "" {
		w.WriteHeader(403)
		fmt.Fprint(w, "<Error><Code>AccessDenied</Code></Error>")
		return
	}
	if f.fail500 > 0 {
		f.fail500--
		w.WriteHeader(500)
		fmt.Fprint(w, "<Error><Code>InternalError</Code></Error>")
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/bkt"), "/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && key == "":
		var keys []string
		for k := range f.objs {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, "<ListBucketResult>")
		for _, k := range keys {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-11-12T03:00:00.000Z</LastModified></Contents>", k, len(f.objs[k]))
		}
		fmt.Fprint(w, "<IsTruncated>false</IsTruncated></ListBucketResult>")
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.uploads["u1"] = map[int][]byte{}
		fmt.Fprint(w, "<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>")
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag%d"`, n))
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		var done struct {
			Parts []struct {
				N    int    `xml:"PartNumber"`
				ETag string `xml:"ETag"`
			} `xml:"Part"`
		}
		xml.Unmarshal(body, &done)
		var all []byte
		for _, p := range done.Parts {
			all = append(all, f.uploads[q.Get("uploadId")][p.N]...)
		}
		f.objs[key] = all
		fmt.Fprint(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodPut:
		f.objs[key] = body
	case r.Method == http.MethodGet:
		b, ok := f.objs[key]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Write(b)
	case r.Method == http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(204)
	}
}

func TestRoundTrip(t *testing.T) {
	fs := &fakeS3{objs: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c, err := New(srv.URL, "bkt", "", "ak", "sk", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	small := filepath.Join(dir, "small")
	os.WriteFile(small, []byte("hello"), 0o600)
	fs.fail500 = 1 // 第一次 500：再试一次
	if err := c.PutFile(ctx, "ferry/p/t/db/db-1.sql.gz.age", small); err != nil {
		t.Fatal(err)
	}
	// 大文件分片：3 片
	c.PartSize, c.MultipartAbove = 4, 8
	big := filepath.Join(dir, "big")
	os.WriteFile(big, []byte("0123456789ab"), 0o600)
	if err := c.PutFile(ctx, "ferry/p/t/db/db-2.sql.gz.age", big); err != nil {
		t.Fatal(err)
	}
	if string(fs.objs["ferry/p/t/db/db-2.sql.gz.age"]) != "0123456789ab" {
		t.Fatalf("%q", fs.objs)
	}
	objs, err := c.List(ctx, "ferry/p/t/db/")
	if err != nil || len(objs) != 2 || objs[1].Size != 12 || objs[0].Modified.IsZero() {
		t.Fatalf("%+v %v", objs, err)
	}
	var b bytes.Buffer
	if n, err := c.Get(ctx, "ferry/p/t/db/db-1.sql.gz.age", &b); err != nil || n != 5 || b.String() != "hello" {
		t.Fatal(n, err, b.String())
	}
	if err := c.Delete(ctx, "ferry/p/t/db/db-1.sql.gz.age"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "ferry/p/t/db/db-1.sql.gz.age", io.Discard); err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Fatal(err)
	}
	// 密钥不对：说清楚
	c.AccessKey = "bad"
	if err := c.PutFile(ctx, "x", small); err == nil || !strings.Contains(err.Error(), "没有这个桶的权限") {
		t.Fatal(err)
	}
}

// TestRealS3 在真的对象存储上试（MinIO：docker run -p 9000:9000 minio/minio server /data）：
// S3C_ENDPOINT=http://127.0.0.1:9000 S3C_BUCKET=test S3C_KEY=minioadmin S3C_SECRET=minioadmin go test -run RealS3 ./internal/s3c/
func TestRealS3(t *testing.T) {
	ep := os.Getenv("S3C_ENDPOINT")
	if ep == "" {
		t.Skip("没有设 S3C_ENDPOINT")
	}
	c, err := New(ep, os.Getenv("S3C_BUCKET"), os.Getenv("S3C_REGION"), os.Getenv("S3C_KEY"), os.Getenv("S3C_SECRET"), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if os.Getenv("S3C_CREATE") != "" { // 先建桶（MinIO 刚起来时没有桶）
		if resp, err := c.do(ctx, http.MethodPut, "", nil, nil, 0, ""); err == nil {
			resp.Body.Close()
		} else if !strings.Contains(err.Error(), "BucketAlready") {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	data := bytes.Repeat([]byte("ferry-backup "), 1<<20) // 13 MB
	p := filepath.Join(dir, "f")
	os.WriteFile(p, data, 0o600)
	c.PartSize, c.MultipartAbove = 5<<20, 6<<20 // S3 的分片最小 5 MB
	for _, key := range []string{"ferry-test/a b+c/one.gz", "ferry-test/multi.gz"} {
		if key == "ferry-test/a b+c/one.gz" {
			c.MultipartAbove = 1 << 30
		} else {
			c.MultipartAbove = 6 << 20
		}
		if err := c.PutFile(ctx, key, p); err != nil {
			t.Fatalf("%s：%v", key, err)
		}
		var b bytes.Buffer
		if _, err := c.Get(ctx, key, &b); err != nil || !bytes.Equal(b.Bytes(), data) {
			t.Fatalf("%s：%v", key, err)
		}
	}
	objs, err := c.List(ctx, "ferry-test/")
	if err != nil || len(objs) != 2 {
		t.Fatalf("%+v %v", objs, err)
	}
	for _, o := range objs {
		if err := c.Delete(ctx, o.Key); err != nil {
			t.Fatal(err)
		}
	}
}
