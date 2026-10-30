package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func TestTLSPrepareAndNeedCert(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root}}
	s := &proto.AppSpec{Name: "site", User: "svc", TLS: &proto.TLSSpec{Domains: []string{"www.example.com"}}}
	dir := a.tlsDir("site")
	if need, why := needCert(dir, s.TLS.Domains, f.now); !need || why != "还没有证书" {
		t.Fatal(why)
	}
	if err := a.tlsPrepare(s); err != nil {
		t.Fatal(err)
	}
	c, err := certInfo(dir)
	if err != nil || c.Subject.CommonName != tempCertName || c.DNSNames[0] != "www.example.com" {
		t.Fatalf("临时证书：%v %+v", err, c)
	}
	if need, why := needCert(dir, s.TLS.Domains, f.now); !need || !strings.Contains(why, "临时证书") {
		t.Fatal("临时证书要换成正式的：" + why)
	}

	// 正式证书：域名一致、离到期还远就不用申请
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mk := func(names []string, notAfter time.Time) {
		tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
			NotBefore: f.now.Add(-time.Hour), NotAfter: notAfter}
		der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		a.writeCert(dir, [][]byte{der}, key, 1000)
	}
	mk([]string{"www.example.com"}, f.now.AddDate(0, 0, 80))
	if need, why := needCert(dir, s.TLS.Domains, f.now); need {
		t.Fatal("还不用续：" + why)
	}
	if need, why := needCert(dir, []string{"www.example.com", "example.com"}, f.now); !need || !strings.Contains(why, "域名变了") {
		t.Fatal(why)
	}
	mk([]string{"www.example.com"}, f.now.AddDate(0, 0, 20))
	if need, why := needCert(dir, s.TLS.Domains, f.now); !need || !strings.Contains(why, "到期") {
		t.Fatal(why)
	}
	// 已有证书时不再放临时证书
	a.tlsPrepare(s)
	if c, _ := certInfo(dir); c.Subject.CommonName == tempCertName {
		t.Fatal("不能覆盖已有的证书")
	}
}

// fakeDNS 记下收到的请求，按 handle 回应。
func fakeDNS(t *testing.T, handle func(r *http.Request, body map[string]any) any) (*httptest.Server, *[]*http.Request) {
	var reqs []*http.Request
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		reqs = append(reqs, r)
		json.NewEncoder(w).Encode(handle(r, body))
	}))
	t.Cleanup(s.Close)
	return s, &reqs
}

func TestDNSAliyun(t *testing.T) {
	s, reqs := fakeDNS(t, func(r *http.Request, _ map[string]any) any {
		switch r.URL.Query().Get("Action") {
		case "GetMainDomainName":
			return map[string]string{"DomainName": "example.com", "RR": "_acme-challenge.www"}
		case "AddDomainRecord":
			return map[string]string{"RecordId": "9001"}
		}
		return map[string]string{}
	})
	p := &aliDNS{id: "AKID", secret: "S", ep: s.URL + "/", c: http.DefaultClient}
	id, err := p.Present(context.Background(), "_acme-challenge.www.example.com", "token-value")
	if err != nil || id != "9001" {
		t.Fatal(id, err)
	}
	q := (*reqs)[1].URL.Query()
	if q.Get("DomainName") != "example.com" || q.Get("RR") != "_acme-challenge.www" || q.Get("Type") != "TXT" || q.Get("Value") != "token-value" ||
		q.Get("AccessKeyId") != "AKID" || q.Get("Signature") == "" {
		t.Fatalf("%v", q)
	}
	if err := p.CleanUp(context.Background(), "", id); err != nil || (*reqs)[2].URL.Query().Get("RecordId") != "9001" {
		t.Fatal(err)
	}
	// 出错时说明阿里云返回的原因
	s2, _ := fakeDNS(t, func(*http.Request, map[string]any) any {
		return map[string]string{"Code": "InvalidAccessKeyId.NotFound", "Message": "Specified access key is not found."}
	})
	p.ep = s2.URL + "/"
	if _, err := p.Present(context.Background(), "_acme-challenge.www.example.com", "v"); err == nil || !strings.Contains(err.Error(), "InvalidAccessKeyId") {
		t.Fatal(err)
	}
}

func TestDNSPod(t *testing.T) {
	s, reqs := fakeDNS(t, func(r *http.Request, _ map[string]any) any {
		switch r.Header.Get("X-TC-Action") {
		case "DescribeDomainList":
			return map[string]any{"Response": map[string]any{"DomainList": []map[string]string{{"Name": "example.com"}, {"Name": "a.example.com"}}}}
		case "CreateRecord":
			return map[string]any{"Response": map[string]any{"RecordId": 123}}
		}
		return map[string]any{"Response": map[string]any{}}
	})
	p := &dnspod{id: "AKID", secret: "S", ep: s.URL + "/", c: http.DefaultClient}
	id, err := p.Present(context.Background(), "_acme-challenge.www.a.example.com", "v")
	if err != nil || id != "a.example.com/123" {
		t.Fatal(id, err)
	}
	if auth := (*reqs)[1].Header.Get("Authorization"); !strings.HasPrefix(auth, "TC3-HMAC-SHA256 Credential=AKID/") {
		t.Fatal(auth)
	}
	if err := p.CleanUp(context.Background(), "", id); err != nil {
		t.Fatal(err)
	}
	s2, _ := fakeDNS(t, func(*http.Request, map[string]any) any {
		return map[string]any{"Response": map[string]any{"Error": map[string]string{"Code": "AuthFailure.SignatureFailure", "Message": "签名不对"}}}
	})
	p.ep = s2.URL + "/"
	if _, err := p.Present(context.Background(), "_acme-challenge.www.example.com", "v"); err == nil || !strings.Contains(err.Error(), "SignatureFailure") {
		t.Fatal(err)
	}
}

func TestDNSCloudflare(t *testing.T) {
	var created map[string]any
	s, reqs := fakeDNS(t, func(r *http.Request, body map[string]any) any {
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("name") == "example.com":
			return map[string]any{"success": true, "result": []map[string]string{{"id": "Z1"}}}
		case r.Method == http.MethodGet:
			return map[string]any{"success": true, "result": []any{}}
		case r.Method == http.MethodPost:
			created = body
			return map[string]any{"success": true, "result": map[string]string{"id": "R1"}}
		}
		return map[string]any{"success": true, "result": nil}
	})
	p := &cloudflare{token: "T", ep: s.URL, c: http.DefaultClient}
	id, err := p.Present(context.Background(), "_acme-challenge.www.example.com", "v")
	if err != nil || id != "Z1/R1" || created["name"] != "_acme-challenge.www.example.com" || created["type"] != "TXT" {
		t.Fatal(id, err, created)
	}
	if (*reqs)[0].Header.Get("Authorization") != "Bearer T" {
		t.Fatal("要带 API Token")
	}
	if err := p.CleanUp(context.Background(), "", id); err != nil {
		t.Fatal(err)
	}
	if last := (*reqs)[len(*reqs)-1]; last.Method != http.MethodDelete || last.URL.Path != "/zones/Z1/dns_records/R1" {
		t.Fatal(last.Method, last.URL)
	}
}

func TestDNSProviderNeedsKeys(t *testing.T) {
	if _, err := newDNSProvider("aliyun", map[string]string{"id": "x"}, nil); err == nil || !strings.Contains(err.Error(), "阿里云 DNS") {
		t.Fatal(err)
	}
	if _, err := newDNSProvider("godaddy", nil, nil); err == nil {
		t.Fatal("不支持的服务商")
	}
}

func TestDNSCredSaved(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root}}
	if _, err := a.loadDNSCred("cloudflare"); err == nil {
		t.Fatal("没存过要报错")
	}
	a.saveDNSCred("cloudflare", map[string]string{"token": "T"})
	if c, err := a.loadDNSCred("cloudflare"); err != nil || c["token"] != "T" {
		t.Fatal(c, err)
	}
}
