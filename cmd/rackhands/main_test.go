package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackman99/rackhands/internal/testbmc"
)

func cfg(t *testing.T) (string, *testbmc.Server) {
	dell := testbmc.Start(t, "dell")
	lenovo := testbmc.Start(t, "lenovo")
	p := filepath.Join(t.TempDir(), "targets.yaml")
	body := fmt.Sprintf(`defaults: {username: u, password: p}
targets:
  - {name: db-01, redfish: %q, allow: [power], tags: [prod]}
  - {name: k8s-12, redfish: %q, tags: [prod]}
`, dell.URL, lenovo.URL)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RACKHANDS_AUDIT_LOG", filepath.Join(t.TempDir(), "a.jsonl"))
	return p, dell
}

func rh(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	code := run(args, strings.NewReader(""), &o, &e)
	return code, o.String(), e.String()
}

func TestReadCommands(t *testing.T) {
	c, _ := cfg(t)
	code, out, _ := rh("--config", c, "health")
	if code != 1 || !strings.Contains(out, "CRITICAL  db-01") || !strings.Contains(out, "OK        k8s-12") {
		t.Fatalf("health exit %d:\n%s", code, out)
	}
	code, out, _ = rh("--config", c, "health", "k8s-12")
	if code != 0 {
		t.Fatalf("healthy target exit %d: %s", code, out)
	}
	code, out, _ = rh("--config", c, "--json", "inventory", "tag:prod")
	if code != 0 || !strings.Contains(out, `"vendor": "Lenovo"`) {
		t.Fatalf("%d %s", code, out)
	}
	code, out, _ = rh("--config", c, "events", "db-01", "-n", "1", "--min", "critical")
	if code != 0 || !strings.Contains(out, "power supply 2 is lost") || strings.Count(out, "\n") != 2 {
		t.Fatalf("%d %s", code, out)
	}
	code, out, _ = rh("--config", c, "sensors", "db-01", "--kind", "psu", "--problems")
	if code != 0 || !strings.Contains(out, "PS2") || strings.Contains(out, "PS1") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _, e := rh("--config", c, "sensors", "nope"); code != 2 || !strings.Contains(e, "unknown target") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestDangerousNeedsAllowAndConfirmation(t *testing.T) {
	c, dell := cfg(t)
	if code, _, e := rh("--config", c, "power", "db-01", "force-off"); code != 3 || !strings.Contains(e, "--allow power") {
		t.Fatalf("no allow: %d %s", code, e)
	}
	if code, _, e := rh("--config", c, "--allow", "power", "power", "db-01", "force-off"); code != 3 || !strings.Contains(e, "--yes db-01") {
		t.Fatalf("no terminal: %d %s", code, e)
	}
	if code, _, _ := rh("--config", c, "--allow", "power", "power", "db-01", "force-off", "--yes", "k8s-12"); code != 3 {
		t.Fatal("wrong --yes accepted")
	}
	if code, _, e := rh("--config", c, "--allow", "power", "power", "k8s-12", "on", "--yes", "k8s-12"); code != 3 || !strings.Contains(e, "allow:") {
		t.Fatalf("target key missing: %d %s", code, e)
	}
	if code, _, _ := rh("--config", c, "--allow", "power", "power", "all", "on", "--yes", "all"); code != 2 {
		t.Fatal("fan-out accepted")
	}
	if code, _, e := rh("--config", c, "--allow", "power", "power", "db-01", "force-off", "--dry-run"); code != 0 || !strings.Contains(e, "dry run") {
		t.Fatalf("dry run %d %s", code, e)
	}
	if dell.Count() != 0 {
		t.Fatal("something was sent before confirmation")
	}
	code, out, e := rh("--config", c, "--allow", "power", "power", "db-01", "force-off", "--yes", "db-01")
	if code != 0 || !strings.Contains(out, "DONE") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	if w, _ := dell.Last(); w.Body["ResetType"] != "ForceOff" {
		t.Fatalf("%+v", w)
	}
}

func TestParse(t *testing.T) {
	o, err := parse([]string{"events", "x", "-n=5", "--allow", "power", "--allow=boot", "--json"})
	if err != nil || o.n != 5 || len(o.allow) != 2 || !o.json || len(o.args) != 2 {
		t.Fatalf("%v %+v", err, o)
	}
	if _, err := parse([]string{"--bogus"}); err == nil {
		t.Fatal("unknown flag")
	}
	if code, out, _ := rh("help"); code != 0 || !strings.Contains(out, "Dangerous") {
		t.Fatal("help")
	}
	if code, _, e := rh("--allow", "reboot", "targets"); code != 2 || !strings.Contains(e, "unknown operation class") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestTLSFingerprintCLI(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer srv.Close()
	code, out, errOut := rh("--host", srv.URL, "tls-fingerprint")
	if code != 0 {
		t.Fatalf("exit %d stderr=%s out=%s", code, errOut, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(out, "tls_fingerprint:") || len(last) != 64 {
		t.Fatalf("expected 64-hex fingerprint line, got %q\nfull:\n%s", last, out)
	}
	code, out, errOut = rh("--host", srv.URL, "--json", "pin")
	if code != 0 || !strings.Contains(out, `"tls_fingerprint"`) {
		t.Fatalf("json pin: %d %s %s", code, out, errOut)
	}
}
