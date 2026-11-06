package engine

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSiteToLocal(t *testing.T) {
	local := "http://nginx-38717.localhost:38717"
	for in, want := range map[string]string{
		"https://shop.example.com/login?x=1": "http://nginx-38717.localhost:38717/login?x=1",
		"http://SHOP.example.com:8080/a":     "http://nginx-38717.localhost:38717/a",
		"https://other.com/x":                "https://other.com/x", // 别的网站不动
		"/relative/path":                     "/relative/path",
	} {
		if got := siteToLocal(in, "shop.example.com", local); got != want {
			t.Errorf("%s → %s，期望 %s", in, got, want)
		}
	}
	if got := localToSite("http://nginx-38717.localhost:38717/p?q", "nginx-38717.localhost:38717", "https://shop.example.com"); got != "https://shop.example.com/p?q" {
		t.Errorf("Referer：%s", got)
	}
	c := reCookieDomain.ReplaceAllString("sid=abc; Path=/; Domain=.shop.example.com; HttpOnly", "")
	if c != "sid=abc; Path=/; HttpOnly" {
		t.Errorf("Cookie：%q", c)
	}
}

func TestStatusCheck(t *testing.T) {
	resp := func(code int) *http.Response {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(""))}
	}
	for code, want := range map[int][2]bool{200: {true, true}, 204: {true, true}, 401: {false, true}, 403: {false, true}, 404: {false, false}, 500: {false, false}} {
		ok, known, _ := statusCheck(resp(code), nil)
		if ok != want[0] || known != want[1] {
			t.Errorf("%d：%v %v", code, ok, known)
		}
	}
	if _, known, err := statusCheck(nil, errors.New("x")); known || err == nil {
		t.Error("出错时不能判断")
	}
}

func TestSortChecked(t *testing.T) {
	in := []WebAccount{{User: "a", Check: "bad"}, {User: "b"}, {User: "c", Check: "ok"}, {User: "d"}}
	var got []string
	for _, a := range sortChecked(in) {
		got = append(got, a.User)
	}
	if strings.Join(got, "") != "cbda" {
		t.Errorf("顺序：%v", got)
	}
}

func TestServerReach(t *testing.T) {
	r := newReach("db", 3306, 33071)
	calls := map[string]int{}
	fn := func(ep reachEP) (bool, error) {
		k := ep.container + ep.host
		calls[k]++
		if ep.container != "" {
			return false, errors.New("timeout") // 容器的地址连不上
		}
		return false, nil // 主机端口连上了，密码不对
	}
	for i := 0; i < 3; i++ {
		r.try(fn)
	}
	if calls["db"] != 1 || calls["127.0.0.1"] != 3 {
		t.Errorf("连不上的地址只试一次：%v", calls)
	}
	if _, err := newReach("", 0, 0).try(fn); err == nil {
		t.Error("没有地址时要报错")
	}
}
