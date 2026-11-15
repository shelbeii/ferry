package engine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/secret"
	"github.com/zalando/go-keyring"
)

func teamTestEngine(t *testing.T) (*Engine, *config.Target, *config.App, map[string]string) {
	t.Helper()
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	target := &config.Target{Name: "prod", SecretsFrom: "file:secrets/prod.age"}
	api := &config.App{Name: "api", Service: config.Service{Secrets: []config.SecretRef{
		{Name: "DB_PASSWORD"}, {Name: "OPTIONAL_KEY", Optional: true},
	}}}
	env := map[string]string{}
	return &Engine{File: &config.File{Project: "demo", Dir: t.TempDir(), Targets: map[string]*config.Target{"prod": target},
		Apps: []*config.App{api}}, Getenv: func(name string) string { return env[name] }}, target, api, env
}

func TestTeamSecretLifecycle(t *testing.T) {
	e, target, api, env := teamTestEngine(t)
	if st, err := e.TeamSecrets("prod"); err != nil || st.Ready || st.Exists || st.PublicKey != "" {
		t.Fatalf("unexpected initial status: %#v, %v", st, err)
	}
	owner, err := EnsureAppAgeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if again, err := EnsureAppAgeIdentity(); err != nil || again != owner {
		t.Fatalf("key generation is not idempotent: %v", err)
	}
	if err := e.InitTeamSecrets("prod"); err != nil {
		t.Fatal(err)
	}
	if err := e.InitTeamSecrets("prod"); err == nil {
		t.Fatal("init overwrote an existing file")
	}
	const value = "test-sample-only with spaces and 漢字"
	if err := e.SaveAppSecret("prod", "api", "DB_PASSWORD", value, false); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.File.Dir, target.AppSecretsFile())
	data, err := os.ReadFile(p)
	if err != nil || !bytes.HasPrefix(data, []byte("age-encryption.org/v1\n")) || bytes.Contains(data, []byte(value)) {
		t.Fatal("team file is not a native age ciphertext")
	}
	member, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	memberKey := member.Recipient().String()
	env[ageIdentityEnv] = member.String()
	if values, err := appSecretValues(target, api, e.appSecretLookup(target)); values != nil || err == nil || !strings.Contains(err.Error(), "不在收件人") {
		t.Fatalf("expected a non-recipient to be denied: %v", err)
	}
	delete(env, ageIdentityEnv)
	if err := e.EditTeamRecipient("prod", memberKey, true); err != nil {
		t.Fatal(err)
	}
	env[ageIdentityEnv] = member.String()
	values, err := appSecretValues(target, api, e.appSecretLookup(target))
	if err != nil || values == nil || (*values)["DB_PASSWORD"] != value {
		t.Fatalf("member could not decrypt the original value: %v", err)
	}
	rows, err := e.AppSecrets("prod", []string{"api"})
	if err != nil || len(rows) != 2 || rows[0].Source != "team" || !rows[0].Available || rows[1].Available {
		t.Fatalf("unexpected status %#v: %v", rows, err)
	}
	env[secret.AppEnv("api", "DB_PASSWORD")] = "ci-override"
	if v, _, _ := e.appSecretLookup(target)("api", "DB_PASSWORD"); v != "ci-override" {
		t.Fatal("team file took precedence over CI variable")
	}
	delete(env, secret.AppEnv("api", "DB_PASSWORD"))
	if err := e.EditTeamRecipient("prod", memberKey, false); err == nil {
		t.Fatal("allowed removing the active identity")
	}
	delete(env, ageIdentityEnv)
	if err := e.EditTeamRecipient("prod", memberKey, false); err != nil {
		t.Fatal(err)
	}
	env[ageIdentityEnv] = member.String()
	if _, _, err := e.appSecretLookup(target)("api", "DB_PASSWORD"); err == nil {
		t.Fatal("removed member still decrypts latest team file")
	}
	delete(env, ageIdentityEnv)
	if err := e.SaveAppSecret("prod", "api", "DB_PASSWORD", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := appSecretValues(target, api, e.appSecretLookup(target)); err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("deleted value remained available: %v", err)
	}
}

func TestTeamSecretUnreadableOptionalAndEnvironment(t *testing.T) {
	e, target, api, env := teamTestEngine(t)
	env[secret.AppEnv("api", "DB_PASSWORD")] = "ci-first"
	env[ageIdentityEnv] = "invalid-private-key-must-not-leak"
	_, err := appSecretValues(target, api, e.appSecretLookup(target))
	var need *AppSecretsRequired
	if !errors.As(err, &need) || len(need.Names) != 1 || need.Names[0] != "OPTIONAL_KEY" || strings.Contains(err.Error(), env[ageIdentityEnv]) {
		t.Fatalf("an unreadable optional value could be lost during a deployment: %v", err)
	}
	env[secret.AppEnv("api", "OPTIONAL_KEY")] = "ci-second"
	values, err := appSecretValues(target, api, e.appSecretLookup(target))
	if err != nil || values == nil || len(*values) != 2 {
		t.Fatalf("complete CI environment should bypass file decryption: %v", err)
	}
}

func TestTeamSecretWritesConcurrently(t *testing.T) {
	e, target, _, _ := teamTestEngine(t)
	if _, err := EnsureAppAgeIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := e.InitTeamSecrets("prod"); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for _, name := range []string{"DB_PASSWORD", "OPTIONAL_KEY"} {
		group.Go(func() {
			if err := e.SaveAppSecret("prod", "api", name, name+"-test-value", false); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	id, err := localAgeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	f, err := e.readTeamSecrets(target, id)
	if err != nil || len(f.Apps["api"]) != 2 {
		t.Fatalf("concurrent edits lost a secret: %v", err)
	}
}

func TestTeamSecretRejectsPathOutsideProject(t *testing.T) {
	e, target, _, _ := teamTestEngine(t)
	if _, err := EnsureAppAgeIdentity(); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.File.Dir, "secrets")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := e.InitTeamSecrets("prod"); err == nil {
		t.Fatal("initialized a team file outside the project")
	}
	if _, err := os.Stat(filepath.Join(outside, "prod.age")); !os.IsNotExist(err) {
		t.Fatalf("unexpected file outside the project: %v", err)
	}
	id, err := localAgeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(outside)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeTeamSecretFile(root, "prod.age", &teamSecretFile{
		Version: 1, Recipients: []string{id.Recipient().String()},
	}, true); err != nil {
		t.Fatal(err)
	}
	if f, err := e.readTeamSecrets(target, id); f != nil || err == nil {
		t.Fatal("read followed a symlink outside the project")
	}
}

func TestTeamSecretInvalidYAMLDoesNotLeak(t *testing.T) {
	e, target, _, _ := teamTestEngine(t)
	envID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var cipher bytes.Buffer
	w, err := age.Encrypt(&cipher, envID.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "unexpected_key_with_secret_contents"
	if _, err := w.Write([]byte(sentinel + ": test\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.File.Dir, target.AppSecretsFile())
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, cipher.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.readTeamSecrets(target, envID); !errors.Is(err, errTeamFormat) || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("expected a sanitized format error: %v", err)
	}
}
