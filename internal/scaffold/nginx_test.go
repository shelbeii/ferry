package scaffold

import (
	"github.com/captain/ferry/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateInstalledNginx(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ferry.yaml")
	s := Spec{ProjectDir: dir, Project: "shop", Targets: []TargetSpec{{Name: "prod", Host: "10.0.0.5", Root: "/opt/shop", User: "root", NginxInstance: "edge"}}, Apps: []AppSpec{{Name: "web", Type: "static", Artifact: "dist", Port: 80, NginxInstance: "${nginxInstance}", Domains: []string{"web.example.test"}}}}
	text, err := Generate(s, p)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte(text), 0o644)
	f, err := config.Load(p)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if f.App("web").Static.NginxInstance != "${nginxInstance}" || f.Targets["prod"].Vars["nginxInstance"] != "edge" || f.App("web").Service.Unit != "" || strings.Contains(text, "runtime:") {
		t.Fatal(text)
	}
}
