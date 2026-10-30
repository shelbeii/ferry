package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/captain/ferry/internal/proto"
)

// HTTPS 证书（static.https）：由 agent 在服务器上用 ACME（默认 Let's Encrypt）申请，
//
//	.ferry/apps/<app>/tls/fullchain.pem、privkey.pem    证书和私钥（root:<运行用户的组>，nginx 能读）
//	.ferry/acme/apps/<app>.json                          续期用的记录（域名、验证方式、要重新加载的单元）
//	.ferry/acme/dns-<服务商>.json                        DNS 验证的密钥（只有 root 能读）
//	.ferry/acme/account-<CA>.pem                         ACME 账号的私钥
//	<ferryDir>/ferry-acme、ferry-acme.timer              每天检查一次，30 天内到期的续期并重新加载 nginx
//
// 证书还没申请到时先放一张自签名的临时证书，nginx 才能启动；申请失败不影响 http 访问。

const (
	certFile     = "fullchain.pem"
	certKeyFile  = "privkey.pem"
	tempCertName = "Ferry 临时证书"
	renewBefore  = 30 * 24 * time.Hour
	acmeTimeout  = 5 * time.Minute
	acmeUnit     = "ferry-acme"
)

func (a *Agent) tlsDir(app string) string { return filepath.Join(a.appMetaDir(app), "tls") }
func (a *Agent) acmeDir() string          { return filepath.Join(a.meta(), "acme") }

// acmeRecord 续期用的记录。
type acmeRecord struct {
	App  string        `json:"app"`
	Unit string        `json:"unit"`
	User string        `json:"user"`
	TLS  proto.TLSSpec `json:"tls"`            // 不含 DNSCred
	Root string        `json:"root,omitempty"` // 应用单独设置的部署根目录
	At   time.Time     `json:"at"`
}

// certInfo 证书文件里第一张证书。
func certInfo(dir string) (*x509.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(dir, certFile))
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("证书文件格式不对")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// needCert 要不要申请：没有证书、是临时证书、域名变了、30 天内到期。
func needCert(dir string, domains []string, now time.Time) (bool, string) {
	c, err := certInfo(dir)
	switch {
	case err != nil:
		return true, "还没有证书"
	case c.Subject.CommonName == tempCertName:
		return true, "现在用的是临时证书"
	case !sameSet(c.DNSNames, domains):
		return true, "域名变了（证书里是 " + strings.Join(c.DNSNames, "、") + "）"
	case c.NotAfter.Sub(now) < renewBefore:
		return true, "证书 " + c.NotAfter.Local().Format("2006-01-02") + " 到期"
	}
	return false, ""
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(x, y)
}

// writeCert 写证书和私钥：归 root、组为运行用户的组（nginx 以运行用户运行，要能读），先写临时文件再改名。
func (a *Agent) writeCert(dir string, chain [][]byte, key *ecdsa.PrivateKey, gid int) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	a.Sys.Lchown(dir, 0, gid)
	os.Chmod(dir, 0o750)
	var certs []byte
	for _, d := range chain {
		certs = append(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	// 私钥先换：nginx 重新加载时证书和私钥要配对，两个都写好才 reload
	for name, data := range map[string][]byte{certKeyFile: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), certFile: certs} {
		p := filepath.Join(dir, name)
		if err := writeFileAtomic(p, data, 0o640); err != nil {
			return err
		}
		a.Sys.Lchown(p, 0, gid)
	}
	return nil
}

// tlsPrepare 启动 nginx 之前：还没有证书时放一张自签名的临时证书（nginx 配置里写了证书文件，没有就起不来）。
func (a *Agent) tlsPrepare(s *proto.AppSpec) error {
	if s.TLS == nil {
		return nil
	}
	dir := a.tlsDir(s.Name)
	if _, err := certInfo(dir); err == nil {
		return nil
	}
	_, gid, err := a.Sys.LookupUser(s.User)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := a.Sys.Now()
	tpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: tempCertName},
		DNSNames: s.TLS.Domains, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	a.info(proto.EvCert, s.Name, "先用自签名的临时证书启动（正式证书在站点起来之后申请）")
	return a.writeCert(dir, [][]byte{der}, key, gid)
}

// tlsAfterStart 站点起来之后：记下续期的配置、装上每天检查的定时器；需要时申请证书、换上、重新加载 nginx。
// 失败只提示：网站照常能用 http 访问，https 先用临时证书。
func (a *Agent) tlsAfterStart(s *proto.AppSpec) {
	t := s.TLS
	if t == nil {
		return
	}
	if len(t.DNSCred) > 0 {
		if err := a.saveDNSCred(t.DNS, t.DNSCred); err != nil {
			a.warn(proto.EvCert, s.Name, "保存 DNS 密钥失败：%v", err)
		}
	}
	rec := acmeRecord{App: s.Name, Unit: s.Unit, User: s.User, TLS: *t, Root: a.appRoots[s.Name], At: a.Sys.Now()}
	rec.TLS.DNSCred = nil
	if err := a.saveRenewal(rec); err != nil {
		a.warn(proto.EvCert, s.Name, "装续期的定时器失败（证书到期前要重新部署一次）：%v", err)
	}
	if err := a.ensureCert(rec, false); err != nil {
		a.warn(proto.EvCert, s.Name, "申请 HTTPS 证书失败：%v。网站照常能用 http 访问，https 先用临时证书（浏览器会提示不安全）；"+
			"服务器每天会再试一次，也可以改好后重新部署", err)
	}
}

// ensureCert 需要时（或 force）申请证书、换上、重新加载 nginx。
func (a *Agent) ensureCert(rec acmeRecord, force bool) error {
	dir := a.tlsDir(rec.App)
	need, why := needCert(dir, rec.TLS.Domains, a.Sys.Now())
	if !need && !force {
		if c, err := certInfo(dir); err == nil {
			a.info(proto.EvCert, rec.App, "证书有效到 %s（%s）", c.NotAfter.Local().Format("2006-01-02"), strings.Join(c.DNSNames, "、"))
		}
		return nil
	}
	if force && !need {
		why = "强制续期"
	}
	how := "HTTP 验证"
	if rec.TLS.DNS != "" {
		how = dnsNames[rec.TLS.DNS] + " DNS 验证"
	}
	a.info(proto.EvCert, rec.App, "申请证书 %s（%s，%s）", strings.Join(rec.TLS.Domains, "、"), why, how)
	var cred map[string]string
	if rec.TLS.DNS != "" {
		var err error
		if cred, err = a.loadDNSCred(rec.TLS.DNS); err != nil {
			return err
		}
	}
	_, gid, err := a.Sys.LookupUser(rec.User)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), acmeTimeout)
	defer cancel()
	if err := a.issueCert(ctx, rec.App, &rec.TLS, cred, dir, gid); err != nil {
		return err
	}
	if err := a.Sys.Systemctl("reload", rec.Unit); err != nil {
		return fmt.Errorf("证书已经申请到，重新加载 nginx 失败：%v", err)
	}
	if c, err := certInfo(dir); err == nil {
		a.info(proto.EvCert, rec.App, "证书已换上：%s，有效到 %s（%s）", strings.Join(c.DNSNames, "、"), c.NotAfter.Local().Format("2006-01-02"), c.Issuer.CommonName)
	}
	return nil
}

// acmeAccountKey ACME 账号的私钥（每个 CA 一个，第一次用时生成）。
func (a *Agent) acmeAccountKey(ca string) (*ecdsa.PrivateKey, error) {
	sum := sha256.Sum256([]byte(ca))
	p := filepath.Join(a.acmeDir(), "account-"+hex.EncodeToString(sum[:6])+".pem")
	if b, err := os.ReadFile(p); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
				return k, nil
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	if err := os.MkdirAll(a.acmeDir(), 0o700); err != nil {
		return nil, err
	}
	return k, writeFileAtomic(p, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

// issueCert 用 ACME 申请证书：每个域名过一遍验证（HTTP 或 DNS），再用新的私钥申请证书。
func (a *Agent) issueCert(ctx context.Context, app string, t *proto.TLSSpec, cred map[string]string, dir string, gid int) error {
	key, err := a.acmeAccountKey(t.CA)
	if err != nil {
		return err
	}
	client := &acme.Client{Key: key, DirectoryURL: t.CA, UserAgent: "ferry"}
	acct := &acme.Account{}
	if t.Email != "" {
		acct.Contact = []string{"mailto:" + t.Email}
	}
	if _, err := client.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return fmt.Errorf("连接证书服务（%s）：%v", t.CA, shortErr(err))
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(t.Domains...))
	if err != nil {
		return fmt.Errorf("申请：%v", shortErr(err))
	}
	var prov dnsProvider
	if t.DNS != "" {
		if prov, err = newDNSProvider(t.DNS, cred, &http.Client{Timeout: 30 * time.Second}); err != nil {
			return err
		}
	}
	want := "http-01"
	if prov != nil {
		want = "dns-01"
	}
	for _, u := range order.AuthzURLs {
		z, err := client.GetAuthorization(ctx, u)
		if err != nil {
			return err
		}
		if z.Status == acme.StatusValid {
			continue
		}
		domain := z.Identifier.Value
		var chal *acme.Challenge
		for _, c := range z.Challenges {
			if c.Type == want {
				chal = c
			}
		}
		if chal == nil {
			return fmt.Errorf("%s 不能用 %s 验证", domain, want)
		}
		var cleanup func()
		if prov != nil {
			cleanup, err = a.presentDNS(ctx, client, prov, app, domain, chal)
		} else {
			cleanup, err = presentHTTP(client, t.Webroot, chal)
		}
		if err != nil {
			return err
		}
		_, err = client.Accept(ctx, chal)
		if err == nil {
			_, err = client.WaitAuthorization(ctx, z.URI)
		}
		cleanup()
		if err != nil {
			hint := ""
			if prov == nil {
				hint = "。HTTP 验证要求：域名已经解析到这台服务器、外网能访问它的 80 端口（国内服务器通常要备案）；不满足时改用 DNS 验证（https.dns）"
			}
			return fmt.Errorf("%s 验证没通过：%v%s", domain, shortErr(err), hint)
		}
		a.info(proto.EvCert, app, "%s 验证通过", domain)
	}
	if order, err = client.WaitOrder(ctx, order.URI); err != nil {
		return fmt.Errorf("申请：%v", shortErr(err))
	}
	ck, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: t.Domains[0]}, DNSNames: t.Domains}, ck)
	if err != nil {
		return err
	}
	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		// 有的证书服务完成订单时不在回应里给订单地址（库就去请求空地址）：订单已经提交了，用已知的订单地址等它签发、取证书
		o, werr := client.WaitOrder(ctx, order.URI)
		if werr != nil || o.CertURL == "" {
			return fmt.Errorf("签发：%v", shortErr(err))
		}
		if chain, err = client.FetchCert(ctx, o.CertURL, true); err != nil {
			return fmt.Errorf("取证书：%v", shortErr(err))
		}
	}
	return a.writeCert(dir, chain, ck, gid)
}

// presentHTTP HTTP 验证：挑战文件放到 <webroot>/.well-known/acme-challenge/<token>（站点的 nginx 指向这里）。
func presentHTTP(client *acme.Client, webroot string, chal *acme.Challenge) (func(), error) {
	if webroot == "" {
		return nil, errors.New("没有放验证文件的目录")
	}
	body, err := client.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(webroot, ".well-known", "acme-challenge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	os.Chmod(filepath.Join(webroot, ".well-known"), 0o755)
	os.Chmod(dir, 0o755)
	p := filepath.Join(dir, chal.Token)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return nil, err
	}
	return func() { os.Remove(p) }, nil
}

// presentDNS DNS 验证：加 TXT 记录，等公共 DNS 查得到再让证书服务来验证；验证完删掉记录。
func (a *Agent) presentDNS(ctx context.Context, client *acme.Client, prov dnsProvider, app, domain string, chal *acme.Challenge) (func(), error) {
	value, err := client.DNS01ChallengeRecord(chal.Token)
	if err != nil {
		return nil, err
	}
	fqdn := "_acme-challenge." + strings.TrimPrefix(domain, "*.")
	id, err := prov.Present(ctx, fqdn, value)
	if err != nil {
		return nil, err
	}
	a.info(proto.EvCert, app, "已加 TXT 记录 %s，等它生效", fqdn)
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := prov.CleanUp(c, fqdn, id); err != nil {
			a.warn(proto.EvCert, app, "删除 TXT 记录 %s 失败（可以手动删掉）：%v", fqdn, err)
		}
	}
	if !waitTXT(ctx, fqdn, value, 3*time.Minute) {
		a.warn(proto.EvCert, app, "公共 DNS 还查不到 %s，照常继续验证", fqdn)
	}
	return cleanup, nil
}

// waitTXT 等公共 DNS（阿里、腾讯、Cloudflare）里任意一个查得到这条 TXT 记录。
func waitTXT(ctx context.Context, fqdn, value string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		for _, srv := range []string{"223.5.5.5:53", "119.29.29.29:53", "1.1.1.1:53"} {
			r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, srv)
			}}
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			txts, _ := r.LookupTXT(c, fqdn)
			cancel()
			if slices.Contains(txts, value) {
				time.Sleep(5 * time.Second) // 证书服务查的服务器可能还慢一点
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
	return false
}

// ---------------------------------------------------------------- 密钥、续期记录

func (a *Agent) dnsCredFile(provider string) string {
	return filepath.Join(a.acmeDir(), "dns-"+provider+".json")
}

func (a *Agent) saveDNSCred(provider string, cred map[string]string) error {
	if _, ok := DNSProviders[provider]; !ok {
		return fmt.Errorf("不支持的 DNS 服务商：%s", provider)
	}
	if err := os.MkdirAll(a.acmeDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(cred)
	return writeFileAtomic(a.dnsCredFile(provider), b, 0o600)
}

func (a *Agent) loadDNSCred(provider string) (map[string]string, error) {
	b, err := os.ReadFile(a.dnsCredFile(provider))
	if err != nil {
		return nil, fmt.Errorf("服务器上没有%s的密钥：在 Ferry 的「设置 → 部署 → HTTPS 证书」里填上后重新部署", dnsNames[provider])
	}
	var cred map[string]string
	if err := json.Unmarshal(b, &cred); err != nil {
		return nil, err
	}
	return cred, nil
}

// saveRenewal 记下续期的配置，把自己复制成 <ferryDir>/ferry-acme，装上每天运行的定时器。
func (a *Agent) saveRenewal(rec acmeRecord) error {
	dir := filepath.Join(a.acmeDir(), "apps")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := writeFileAtomic(filepath.Join(dir, rec.App+".json"), b, 0o600); err != nil {
		return err
	}
	// 记下这个部署目录（一台服务器上可以有好几个项目）
	roots := filepath.Join(a.ferryDir(), "acme-roots")
	have, _ := os.ReadFile(roots)
	if !slices.Contains(strings.Split(strings.TrimSpace(string(have)), "\n"), a.req.Root) {
		if err := writeFileAtomic(roots, []byte(strings.TrimSpace(string(have)+"\n"+a.req.Root)+"\n"), 0o644); err != nil {
			return err
		}
	}
	dst, err := a.selfCopy(acmeUnit)
	if err != nil {
		return err
	}
	svc := ManagedMarker + " — 由 ferry 生成：检查 HTTPS 证书，30 天内到期的续期\n[Unit]\nDescription=Ferry HTTPS 证书续期\nAfter=network-online.target\nWants=network-online.target\n\n" +
		"[Service]\nType=oneshot\nExecStart=" + dst + " acme-renew\n"
	timer := ManagedMarker + " — 由 ferry 生成\n[Unit]\nDescription=Ferry HTTPS 证书续期（每天）\n\n[Timer]\nOnCalendar=daily\nRandomizedDelaySec=3600\nPersistent=true\n\n" +
		"[Install]\nWantedBy=timers.target\n"
	if err := a.installUnit(rec.App, acmeUnit+".service", []byte(svc)); err != nil {
		return err
	}
	if err := a.installUnit(rec.App, acmeUnit+".timer", []byte(timer)); err != nil {
		return err
	}
	return a.Sys.Systemctl("enable", "--now", acmeUnit+".timer")
}

func sameContent(a, b string) bool {
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// RenewAll ferry-acme acme-renew：定时器每天运行，检查记下的每个站点的证书，30 天内到期的续期（force：都重新申请）。
// 部署目录从 <自己所在的目录>/acme-roots 读。输出给 journald 看。
func RenewAll(sys System, out io.Writer, force bool) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), "acme-roots"))
	rc := 0
	for _, root := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if root == "" {
			continue
		}
		recs, _ := filepath.Glob(filepath.Join(root, ".ferry", "acme", "apps", "*.json"))
		for _, f := range recs {
			var rec acmeRecord
			data, err := os.ReadFile(f)
			if err != nil || json.Unmarshal(data, &rec) != nil {
				fmt.Fprintf(out, "%s：读不了续期记录\n", f)
				rc = 1
				continue
			}
			a := &Agent{Sys: sys, Out: out, Plain: true, req: &proto.Request{Root: root, FerryDir: filepath.Dir(exe)}, appRoots: map[string]string{}}
			if rec.Root != "" {
				a.appRoots[rec.App] = rec.Root
			}
			if err := a.ensureCert(rec, force); err != nil {
				fmt.Fprintf(out, "%s %s：续期失败：%v\n", root, rec.App, err)
				rc = 1
			}
		}
	}
	return rc
}
