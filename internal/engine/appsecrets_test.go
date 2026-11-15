package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/zalando/go-keyring"
)

func TestAppSecretValues(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	api := &config.App{Name: "shop-api", Type: "binary", Service: config.Service{Secrets: []config.SecretRef{
		{Name: "DB_PASSWORD"}, {Name: "SMS_KEY", Optional: true},
	}}}
	prod := &config.Target{Name: "prod"}
	test := &config.Target{Name: "test"}
	env := map[string]string{}
	e := &Engine{File: &config.File{Project: "demo", Apps: []*config.App{api}, Targets: map[string]*config.Target{"prod": prod, "test": test}},
		Getenv: func(key string) string { return env[key] }}

	_, err := appSecretValues(prod, api, e.appSecretLookup(prod))
	var missing *AppSecretsRequired
	if !errors.As(err, &missing) || missing.Target != "prod" || missing.App != api.Name ||
		strings.Join(missing.Names, ",") != "DB_PASSWORD" || missing.envNames()[0] != "FERRY_SECRET_SHOP_API_DB_PASSWORD" {
		t.Fatalf("missing required value: %v", err)
	}
	for _, item := range []struct {
		value  string
		shared bool
	}{{"fallback-demo", true}, {"target-demo", false}} {
		if err := e.SaveAppSecret("prod", api.Name, "DB_PASSWORD", item.value, item.shared); err != nil {
			t.Fatal(err)
		}
	}
	check := func(target *config.Target, want string) {
		t.Helper()
		values, err := appSecretValues(target, api, e.appSecretLookup(target))
		if err != nil || len(*values) != 1 || (*values)["DB_PASSWORD"] != want {
			t.Fatalf("unexpected lookup result for %s: %v", target.Name, err)
		}
	}
	check(prod, "target-demo")
	check(test, "fallback-demo")
	env["FERRY_SECRET_SHOP_API_DB_PASSWORD"] = "demo-ci"
	check(prod, "demo-ci")
	env["FERRY_SECRET_SHOP_API_DB_PASSWORD"] = "invalid-value\nnext line"
	if _, err := appSecretValues(prod, api, e.appSecretLookup(prod)); err == nil || strings.Contains(err.Error(), "invalid-value") {
		t.Fatalf("invalid value must fail safely: %v", err)
	}
	rows, err := e.AppSecrets("prod", []string{api.Name})
	if err != nil || len(rows) != 2 || rows[0].Source != "invalid" || rows[0].Available || rows[1].Available {
		t.Fatalf("unexpected status: %+v %v", rows, err)
	}
}

func TestAppSecretRequestFormatting(t *testing.T) {
	values := proto.AppSecretValues{"DB_PASSWORD": "dummy-value-should-only-travel-over-ssh"}
	req := &proto.Request{Apps: []proto.AppSpec{{Name: "api", Secrets: &values}}}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, req), values["DB_PASSWORD"]) {
			t.Fatalf("%s formatted a request with the value", format)
		}
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var decoded proto.Request
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if (*decoded.Apps[0].Secrets)["DB_PASSWORD"] != values["DB_PASSWORD"] {
		t.Fatal("SSH request did not carry the input value")
	}
	empty := proto.AppSecretValues{}
	b, err = json.Marshal(proto.AppSpec{Secrets: &empty})
	if err != nil || !strings.Contains(string(b), `"secrets":{}`) {
		t.Fatal("empty optional-only payload must be distinguishable from undeclared")
	}
}
