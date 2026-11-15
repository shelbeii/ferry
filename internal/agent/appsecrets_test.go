package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestAppSecretsFileAndDiagnostics(t *testing.T) {
	value := "  dummy '\"$VAR \\ `  "
	data, names := appSecretsContent(proto.AppSecretValues{"TEST_KEY": value, "ANOTHER_KEY": "extra"})
	if strings.Join(names, ",") != "ANOTHER_KEY,TEST_KEY" || strings.Join(appSecretNames(data), ",") != strings.Join(names, ",") {
		t.Fatal("expected sorted names in the file")
	}
	if !strings.Contains(string(data), "TEST_KEY=\"  dummy '\\\"\\$VAR \\\\ \\`  \"\n") {
		t.Fatal("systemd quoting did not preserve literal characters")
	}
	file := filepath.Join(t.TempDir(), proto.AppSecretsFile)
	if err := writeAppSecrets(file, data); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected a file with mode 0600: %v", err)
	}
	if validLogPath(proto.AppSecretsFile) || len((&logSource{Base: filepath.Dir(file)}).match(".*")) > 0 {
		t.Fatal("application logs exposed the managed file")
	}
}

func TestSlotSecretsUnit(t *testing.T) {
	unit := "[Unit]\n\n[Service]\nEnvironmentFile=/etc/example\n\n[Install]\nWantedBy=multi-user.target\n"
	slot := "/opt/example/apps/api/slots/b"
	added := slotSecretsUnit(unit, slot, true)
	if !strings.Contains(added, "EnvironmentFile=/etc/example\n"+proto.AppSecretsDirective(slot)+"\n") {
		t.Fatal("the slot's env file should be read after user files")
	}
	if removed := slotSecretsUnit(added, slot, false); removed != unit {
		t.Fatal("rollback to a pre-secrets release should preserve other directives")
	}
}
