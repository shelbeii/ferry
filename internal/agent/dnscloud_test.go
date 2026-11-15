package agent

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestCloudDNSPreservesExistingTXT(t *testing.T) {
	const name = "_acme-challenge.api.example.com."
	for _, provider := range []string{"huaweicloud", "route53"} {
		t.Run(provider, func(t *testing.T) {
			route53 := provider == "route53"
			values := []string{`"existing"`}
			var valuesMu sync.Mutex
			box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				valuesMu.Lock()
				defer valuesMu.Unlock()
				auth := r.Header.Get("Authorization")
				if route53 {
					if !strings.Contains(auth, "/us-east-1/route53/aws4_request") || r.Header.Get("X-Amz-Date") == "" {
						t.Error("缺少 Route 53 请求签名")
					}
				} else if !strings.HasPrefix(auth, "SDK-HMAC-SHA256 Access=id,") || r.Header.Get("X-Sdk-Date") == "" {
					t.Error("缺少华为云请求签名")
				}
				q := r.URL.Query()
				switch r.URL.Path {
				case "/hostedzonesbyname":
					if q.Get("dnsname") == "example.com." {
						w.Write([]byte(`<R><HostedZones><HostedZone><Id>/hostedzone/PRIVATE</Id><Name>example.com.</Name><Config><PrivateZone>true</PrivateZone></Config></HostedZone><HostedZone><Id>/hostedzone/PUBLIC</Id><Name>example.com.</Name></HostedZone></HostedZones></R>`))
					} else {
						w.Write([]byte(`<R />`))
					}
				case "/v2/zones":
					if q.Get("name") == "example.com." && q.Get("type") == "public" {
						w.Write([]byte(`{"zones":[{"id":"PUBLIC","name":"example.com."}]}`))
					} else {
						w.Write([]byte(`{"zones":[]}`))
					}
				case "/hostedzone/PUBLIC/rrset":
					if r.Method == "GET" {
						xml.NewEncoder(w).Encode(struct {
							XMLName xml.Name        `xml:"ListResourceRecordSetsResponse"`
							Records []route53Record `xml:"ResourceRecordSets>ResourceRecordSet"`
						}{Records: []route53Record{{Name: name, Type: "TXT", TTL: 120, Values: values}}})
					} else {
						var req struct {
							Action string        `xml:"ChangeBatch>Changes>Change>Action"`
							Record route53Record `xml:"ChangeBatch>Changes>Change>ResourceRecordSet"`
						}
						err := xml.NewDecoder(r.Body).Decode(&req)
						if err != nil || req.Action != "UPSERT" || req.Record.Name != name || req.Record.TTL != 120 {
							t.Errorf("请求：%+v %v", req, err)
						}
						values = req.Record.Values
					}
				case "/v2/zones/PUBLIC/recordsets":
					json.NewEncoder(w).Encode(map[string]any{"recordsets": []huaweiRecord{{ID: "existing", Name: name, Type: "TXT", TTL: 120, Records: values}}})
				case "/v2/zones/PUBLIC/recordsets/existing":
					var req huaweiRecord
					err := json.NewDecoder(r.Body).Decode(&req)
					if err != nil || r.Method != "PUT" || req.Name != name || req.TTL != 120 {
						t.Errorf("请求：%+v %v", req, err)
					}
					values = req.Records
				default:
					t.Errorf("意外请求 %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer box.Close()
			p := &cloudDNS{provider: provider, id: "id", secret: "secret", endpoint: box.URL, client: box.Client()}
			readValues := func() []string {
				valuesMu.Lock()
				defer valuesMu.Unlock()
				return append([]string(nil), values...)
			}
			fqdn := strings.TrimSuffix(name, ".")
			id, err := p.Present(t.Context(), fqdn, "challenge")
			if got := readValues(); err != nil || !reflect.DeepEqual(got, []string{`"existing"`, `"challenge"`}) {
				t.Fatalf("添加挑战：%q %v", got, err)
			}
			err = p.CleanUp(t.Context(), fqdn, id)
			if got := readValues(); err != nil || !reflect.DeepEqual(got, []string{`"existing"`}) {
				t.Fatalf("清理挑战：%q %v", got, err)
			}
		})
	}
}
