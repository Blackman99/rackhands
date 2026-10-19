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
	"time"

	"github.com/Blackman99/rackhands/internal/testbmc"
)

func cfg(t *testing.T) (string, *testbmc.Server) {
	dell := testbmc.Start(t, "dell")
	lenovo := testbmc.Start(t, "lenovo")
	p := filepath.Join(t.TempDir(), "targets.yaml")
	body := fmt.Sprintf(`defaults: {username: u, password: p}
targets:
  - {name: db-01, redfish: %q, allow: [power, firmware], tags: [prod]}
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

func fastPolls(t *testing.T) {
	e, m := pollEvery, pollMax
	pollEvery, pollMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { pollEvery, pollMax = e, m })
}

func TestFirmwareUpdateWait(t *testing.T) {
	fastPolls(t)
	c, dell := cfg(t)
	code, out, e := rh("--config", c, "--allow", "firmware", "firmware-update", "db-01", "https://repo.example/iDRAC.EXE", "--yes", "db-01", "--wait")
	t.Logf("stdout:\n%s\nstderr:\n%s", out, e)
	if code != 0 || !strings.HasPrefix(out, "DONE  db-01: JID_001 completed") || !strings.Contains(out, "Job completed successfully.") {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"then watch the task it starts", "SENT  db-01: accepted (HTTP 202)", "watching JID_001", "WAIT  db-01 JID_001  Downloading", "Running      40% ████░░░░░░  Installing"} {
		if !strings.Contains(e, want) {
			t.Fatalf("stderr lacks %q", want)
		}
	}
	if dell.Count() != 1 {
		t.Fatalf("watching must only read: %d writes", dell.Count())
	}
	// without --wait: say how to follow it
	code, _, e = rh("--config", c, "--allow", "firmware", "firmware-update", "db-01", "https://repo.example/BIOS.EXE", "--yes", "db-01")
	if code != 0 || !strings.Contains(e, "follow it: rackhands --config "+c+" task db-01 JID_001 --wait") {
		t.Fatalf("%d %s", code, e)
	}
	// --wait never skips a gate
	if code, _, e := rh("--config", c, "firmware-update", "db-01", "https://x/y", "--yes", "db-01", "--wait"); code != 3 || !strings.Contains(e, "--allow firmware") {
		t.Fatalf("--wait bypassed --allow: %d %s", code, e)
	}
	if code, _, e := rh("--config", c, "--allow", "firmware", "firmware-update", "k8s-12", "https://x/y", "--yes", "k8s-12", "--wait"); code != 3 || !strings.Contains(e, "allow:") {
		t.Fatalf("--wait bypassed the target key: %d %s", code, e)
	}
	if code, _, e := rh("--config", c, "--allow", "firmware", "firmware-update", "db-01", "https://x/y", "--wait"); code != 3 || !strings.Contains(e, "--yes db-01") {
		t.Fatalf("--wait bypassed confirmation: %d %s", code, e)
	}
	code, out, e = rh("--config", c, "--allow", "firmware", "--json", "firmware-update", "db-01", "https://x/y", "--yes", "db-01", "--wait")
	if code != 0 || !strings.Contains(out, `"verdict": "completed"`) || strings.Contains(e, "WAIT") {
		t.Fatalf("json: %d %s %s", code, out, e)
	}
}

func TestTaskCommand(t *testing.T) {
	fastPolls(t)
	c, dell := cfg(t)
	code, out, e := rh("--config", c, "task", "db-01")
	t.Logf("list:\n%s%s", out, e)
	if code != 0 || !strings.Contains(out, "JID_003  iDRAC job  Scheduled  0%    Firmware Update: BIOS") {
		t.Fatalf("exit %d", code)
	}
	code, out, e = rh("--config", c, "task", "db-01", "JID_002")
	t.Logf("show:\n%s%s", out, e)
	if code != 0 || !strings.Contains(out, "Unable to verify Update Package signature.") {
		t.Fatalf("exit %d", code)
	}
	code, out, e = rh("--config", c, "task", "db-01", "JID_002", "--wait")
	t.Logf("failed:\n%s%s", out, e)
	if code != 4 || !strings.HasPrefix(out, "FAILED  db-01: JID_002 failed — Unable to verify Update Package signature.") {
		t.Fatalf("exit %d", code)
	}
	code, out, e = rh("--config", c, "task", "db-01", "JID_003", "--wait")
	t.Logf("staged:\n%s%s", out, e)
	if code != 0 || !strings.HasPrefix(out, "STAGED  db-01: JID_003 is Scheduled — Task successfully scheduled.") || !strings.Contains(out, "power power db-01 restart") {
		t.Fatalf("exit %d", code)
	}
	code, out, e = rh("--config", c, "task", "k8s-12", "/redfish/v1/TaskService/TaskMonitors/1", "--wait")
	t.Logf("monitor:\n%s%s", out, e)
	if code != 0 || !strings.HasPrefix(out, "DONE  k8s-12: task 1 completed in 6m41s — The target device 'UEFI'") {
		t.Fatalf("exit %d", code)
	}
	code, out, e = rh("--config", c, "task", "db-01", "JID_404")
	t.Logf("unknown:\n%s%s", out, e)
	if code != 2 || !strings.Contains(e, "task not found: no JID_404 on this BMC; list them with `rackhands task db-01`") {
		t.Fatalf("exit %d", code)
	}
	if dell.Count() != 0 {
		t.Fatal("task is read-only")
	}
	for _, bad := range [][]string{
		{"task", "db-01", "--wait"},
		{"task"},
		{"power", "db-01", "--wait"},
		{"task", "db-01", "JID_001", "--wait-timeout", "soon"},
		{"task", "all", "JID_001"},
	} {
		code, _, e := rh(append([]string{"--config", c}, bad...)...)
		t.Logf("%v -> %d %s", bad, code, e)
		if code != 2 || e == "" {
			t.Fatalf("%v: exit %d", bad, code)
		}
	}
}

func TestTaskWaitTimeout(t *testing.T) {
	pollEvery, pollMax = time.Second, time.Second
	t.Cleanup(func() { pollEvery, pollMax = 2*time.Second, 15*time.Second })
	c, _ := cfg(t)
	start := time.Now()
	code, out, e := rh("--config", c, "task", "k8s-12", "1", "--wait-timeout", "300ms")
	t.Logf("timeout:\n%s%s", out, e)
	if code != 4 || !strings.HasPrefix(out, "TIMEOUT  k8s-12: task 1 is still Running at 5% at the 300ms deadline.") || time.Since(start) > 3*time.Second {
		t.Fatalf("exit %d after %v", code, time.Since(start))
	}
}
