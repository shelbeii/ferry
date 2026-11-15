package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type cloudDNS struct {
	provider, id, secret, endpoint string
	client                         *http.Client
}

var cloudDNSID = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

// 两家 API 都按记录集操作；保留同名记录中的其他 TXT 值。
type cloudTXT struct {
	zone, id string
	ttl      int
	values   []string
}

func (p *cloudDNS) Present(ctx context.Context, fqdn, value string) (string, error) {
	name := fqdn + "."
	zone, err := p.zone(ctx, name)
	if err != nil {
		return "", err
	}
	txt, err := p.txt(ctx, zone, name)
	if err != nil {
		return "", err
	}
	record := strconv.Quote(value)
	if slices.Contains(txt.values, record) {
		return "", nil
	}
	if err = p.write(ctx, name, txt, append(txt.values, record)); err != nil {
		return "", err
	}
	return zone + ":" + record, nil
}

func (p *cloudDNS) CleanUp(ctx context.Context, fqdn, id string) error {
	if id == "" {
		return nil
	}
	zone, value, ok := strings.Cut(id, ":")
	if !ok || !cloudDNSID.MatchString(zone) {
		return errors.New("无效的 DNS 验证记录标识")
	}
	name := fqdn + "."
	txt, err := p.txt(ctx, zone, name)
	if err != nil {
		return err
	}
	index := slices.Index(txt.values, value)
	if index < 0 {
		return nil
	}
	values := append(slices.Clone(txt.values[:index]), txt.values[index+1:]...)
	return p.write(ctx, name, txt, values)
}

func (p *cloudDNS) zone(ctx context.Context, name string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		suffix := strings.Join(labels[i:], ".") + "."
		if p.provider == "route53" {
			q := url.Values{"dnsname": {suffix}}
			for {
				var result struct {
					Zones []struct {
						ID      string `xml:"Id"`
						Name    string
						Private bool `xml:"Config>PrivateZone"`
					} `xml:"HostedZones>HostedZone"`
					NextName string `xml:"NextDNSName"`
					NextID   string `xml:"NextHostedZoneId"`
				}
				if err := p.call(ctx, "GET", "/hostedzonesbyname", q, nil, &result); err != nil {
					return "", err
				}
				for _, z := range result.Zones {
					id := strings.TrimPrefix(z.ID, "/hostedzone/")
					if z.Name == suffix && !z.Private && cloudDNSID.MatchString(id) {
						return id, nil
					}
				}
				if result.NextName != suffix || result.NextID == "" || result.NextID == q.Get("hostedzoneid") {
					break
				}
				q.Set("hostedzoneid", result.NextID)
			}
		} else {
			var result struct{ Zones []struct{ ID, Name string } }
			if err := p.call(ctx, "GET", "/v2/zones", url.Values{"name": {suffix}, "type": {"public"}}, nil, &result); err != nil {
				return "", err
			}
			for _, z := range result.Zones {
				if z.Name == suffix && cloudDNSID.MatchString(z.ID) {
					return z.ID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%s 没有找到 %s 的公网托管区", dnsNames[p.provider], name)
}

type route53Record struct {
	Name   string
	Type   string
	TTL    int
	Values []string `xml:"ResourceRecords>ResourceRecord>Value"`
	Policy string   `xml:"SetIdentifier,omitempty"`
}

type huaweiRecord struct {
	ID      string   `json:"id,omitempty"`
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int      `json:"ttl"`
	Records []string `json:"records"`
}

func (p *cloudDNS) txt(ctx context.Context, zone, name string) (cloudTXT, error) {
	txt := cloudTXT{zone: zone, ttl: 60}
	q := url.Values{"name": {name}, "type": {"TXT"}}
	if p.provider == "route53" {
		q.Set("maxitems", "1")
		var result struct {
			Records []route53Record `xml:"ResourceRecordSets>ResourceRecordSet"`
		}
		if err := p.call(ctx, "GET", "/hostedzone/"+zone+"/rrset", q, nil, &result); err != nil {
			return txt, err
		}
		for _, r := range result.Records {
			if r.Name == name && r.Type == "TXT" {
				if r.Policy != "" {
					return txt, errors.New("Route 53 验证记录使用了特殊路由策略，请为 ACME 使用普通 TXT 记录")
				}
				txt.ttl, txt.values = r.TTL, r.Values
			}
		}
	} else {
		var result struct {
			Records []huaweiRecord `json:"recordsets"`
		}
		if err := p.call(ctx, "GET", "/v2/zones/"+zone+"/recordsets", q, nil, &result); err != nil {
			return txt, err
		}
		for _, r := range result.Records {
			if r.Name == name && r.Type == "TXT" && cloudDNSID.MatchString(r.ID) {
				txt.id, txt.ttl, txt.values = r.ID, r.TTL, r.Records
				break
			}
		}
	}
	return txt, nil
}

func (p *cloudDNS) write(ctx context.Context, name string, txt cloudTXT, values []string) error {
	if p.provider == "route53" {
		change := struct {
			XMLName xml.Name      `xml:"ChangeResourceRecordSetsRequest"`
			XMLNS   string        `xml:"xmlns,attr"`
			Action  string        `xml:"ChangeBatch>Changes>Change>Action"`
			Record  route53Record `xml:"ChangeBatch>Changes>Change>ResourceRecordSet"`
		}{XMLNS: "https://route53.amazonaws.com/doc/2013-04-01/", Action: "UPSERT",
			Record: route53Record{Name: name, Type: "TXT", TTL: txt.ttl, Values: values}}
		if len(values) == 0 {
			change.Action, change.Record.Values = "DELETE", txt.values
		}
		body, err := xml.Marshal(change)
		if err != nil {
			return err
		}
		return p.call(ctx, "POST", "/hostedzone/"+txt.zone+"/rrset", nil, body, nil)
	}
	endpoint := "/v2/zones/" + txt.zone + "/recordsets"
	if len(values) == 0 {
		return p.call(ctx, "DELETE", endpoint+"/"+txt.id, nil, nil, nil)
	}
	method := "POST"
	if txt.id != "" {
		endpoint, method = endpoint+"/"+txt.id, "PUT"
	}
	body, err := json.Marshal(huaweiRecord{Name: name, Type: "TXT", TTL: txt.ttl, Records: values})
	if err != nil {
		return err
	}
	return p.call(ctx, method, endpoint, nil, body, nil)
}

func (p *cloudDNS) call(ctx context.Context, method, path string, q url.Values, body []byte, out any) error {
	u := p.endpoint + path
	query := strings.ReplaceAll(q.Encode(), "+", "%20")
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	payload := sha256hex(body)
	stamp := now.Format("20060102T150405Z")
	if p.provider == "route53" {
		req.Header.Set("Content-Type", "application/xml")
		req.Header.Set("X-Amz-Date", stamp)
		day := stamp[:8]
		scope := day + "/us-east-1/route53/aws4_request"
		headers := "host:" + req.URL.Host + "\nx-amz-date:" + stamp + "\n"
		signed := "host;x-amz-date"
		canonical := strings.Join([]string{method, req.URL.EscapedPath(), query, headers, signed, payload}, "\n")
		input := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + sha256hex([]byte(canonical))
		key := hmac256(hmac256(hmac256(hmac256([]byte("AWS4"+p.secret), day), "us-east-1"), "route53"), "aws4_request")
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+p.id+"/"+scope+", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmac256(key, input)))
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Sdk-Date", stamp)
		headers := "content-type:application/json\nhost:" + req.URL.Host + "\nx-sdk-date:" + stamp + "\n"
		signed := "content-type;host;x-sdk-date"
		canonical := strings.Join([]string{method, req.URL.EscapedPath() + "/", query, headers, signed, payload}, "\n")
		input := "SDK-HMAC-SHA256\n" + stamp + "\n" + sha256hex([]byte(canonical))
		req.Header.Set("Authorization", "SDK-HMAC-SHA256 Access="+p.id+", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmac256([]byte(p.secret), input)))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r := io.LimitReader(resp.Body, 4<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(r, 1024))
		return fmt.Errorf("%s：HTTP %d %s", dnsNames[p.provider], resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		io.Copy(io.Discard, r)
		return nil
	}
	if p.provider == "route53" {
		return xml.NewDecoder(r).Decode(out)
	}
	return json.NewDecoder(r).Decode(out)
}
