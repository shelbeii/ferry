package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// 阿里云文档里的签名示例（DescribeDomainRecords）。
func TestAliSignDocExample(t *testing.T) {
	q := aliSign(map[string]string{
		"Format": "XML", "AccessKeyId": "testid", "Action": "DescribeDomainRecords", "SignatureMethod": "HMAC-SHA1",
		"DomainName": "example.com", "SignatureNonce": "f59ed6a9-83fc-473b-9cc6-99c95df3856e", "SignatureVersion": "1.0",
		"Version": "2015-01-09", "Timestamp": "2016-03-24T16:41:54Z",
	}, "testsecret")
	if want := "Signature=uRpHwaSEt3J%2B6KQD%2F%2FsvCh%2Fx%2BpI%3D"; !strings.HasSuffix(q, want) {
		t.Fatalf("%s", q)
	}
}

// 腾讯云 API 3.0 文档里的示例（cvm DescribeInstances）：规范请求的写法与文档给出的哈希一致。
// 文档里的密钥是打码的，最后的签名没法对，只能对到规范请求为止；后面的密钥派生按文档的步骤（下面单独核对）。
func TestTC3CanonDocExample(t *testing.T) {
	canon := tc3Canon("cvm.tencentcloudapi.com", "DescribeInstances", "35e9c5b0e3ae67532d3c9f17ead6c90222632e5b1ff7f6e89887f1398934f064")
	if got := sha256hex([]byte(canon)); got != "7019a55be8395899b900fb5564e4200d984910f34794a27cb3fb7d10ff6a1e84" {
		t.Fatalf("规范请求的哈希：%s\n%s", got, canon)
	}
}

// 密钥派生：SecretDate = HMAC("TC3"+SecretKey, Date)，SecretService = HMAC(SecretDate, Service)，
// SecretSigning = HMAC(SecretService, "tc3_request")，Signature = hex(HMAC(SecretSigning, StringToSign))。
func TestTC3SignSteps(t *testing.T) {
	body := []byte(`{"Domain":"example.com"}`)
	auth := tc3Auth("AKIDtest", "secret", "dnspod.tencentcloudapi.com", "dnspod", "DescribeDomainList", 1551113065, body)
	toSign := "TC3-HMAC-SHA256\n1551113065\n2019-02-25/dnspod/tc3_request\n" +
		sha256hex([]byte(tc3Canon("dnspod.tencentcloudapi.com", "DescribeDomainList", sha256hex(body))))
	h := func(k []byte, m string) []byte { x := hmac.New(sha256.New, k); x.Write([]byte(m)); return x.Sum(nil) }
	sig := hex.EncodeToString(h(h(h(h([]byte("TC3secret"), "2019-02-25"), "dnspod"), "tc3_request"), toSign))
	want := "TC3-HMAC-SHA256 Credential=AKIDtest/2019-02-25/dnspod/tc3_request, SignedHeaders=content-type;host;x-tc-action, Signature=" + sig
	if auth != want {
		t.Fatalf("\n%s\n%s", auth, want)
	}
}
