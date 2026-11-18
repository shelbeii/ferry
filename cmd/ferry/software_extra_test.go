package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestInstallExtras(t *testing.T) {
	c, err := parseArgs([]string{"software", "install", "prod", "mysql", "--conf", "innodb_buffer_pool_size=1G", "--conf=sql_mode=A,B", "--conf", "mysqld:max_connections=900", "--set", "public=true"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := installExtras(`{"default-time-zone": "+08:00", "mysqld": {"skip-name-resolve": ""}}`, c.flags["conf"])
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"":       {"innodb_buffer_pool_size": "1G", "sql_mode": "A,B", "default-time-zone": "+08:00"},
		"mysqld": {"max_connections": "900", "skip-name-resolve": ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v\n要 %v", got, want)
	}
	if got, err := installExtras("", ""); err != nil || got != nil {
		t.Errorf("没写：%v %v", got, err)
	}
	for _, c := range []struct{ js, lines, want string }{
		{"[1]", "", "要写成 JSON 对象"},
		{`{"a": {"b": [1]}}`, "", "要写成文字或数字"},
		{"", "novalue", "名称=值"},
		{`{"a": "1"}`, "a=2", "写了两次"},
	} {
		if _, err := installExtras(c.js, c.lines); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q %q：%v", c.js, c.lines, err)
		}
	}
}
