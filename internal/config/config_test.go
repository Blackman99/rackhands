package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "t.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndSelect(t *testing.T) {
	p := write(t, `
defaults:
  username: monitor
  password_env: RH_TEST_PW
  insecure: false
targets:
  - {name: a, redfish: "https://10.0.0.1", tags: [db]}
  - {name: b, ipmi: "10.0.0.2:6230", tags: [db, lab], allow: [power]}
  - {name: c, redfish: "10.0.0.3", username: root, password: x}
`)
	t.Setenv("RH_TEST_PW", "s3cret")
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := f.Find("a")
	if a.Username != "monitor" || a.InsecureTLS() {
		t.Fatalf("defaults not applied: %+v", a)
	}
	if pw, _ := a.Secret(); pw != "s3cret" {
		t.Fatal("password_env")
	}
	c, _ := f.Find("c")
	if pw, _ := c.Secret(); pw != "x" || c.Username != "root" {
		t.Fatal("inline override")
	}
	b, _ := f.Find("b")
	if h, port := b.IPMIAddr(); h != "10.0.0.2" || port != 6230 {
		t.Fatalf("ipmi addr %s %d", h, port)
	}
	sel, err := f.Select([]string{"tag:db"})
	if err != nil || len(sel) != 2 {
		t.Fatalf("tag select: %v %d", err, len(sel))
	}
	sel, _ = f.Select([]string{"all", "a"})
	if len(sel) != 3 {
		t.Fatal("dedupe")
	}
	if _, err := f.Select([]string{"nope"}); err == nil {
		t.Fatal("unknown target")
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{
		"targets: [{redfish: x}]",
		"targets: [{name: a}]",
		"targets: [{name: a, redfish: x}, {name: a, redfish: y}]",
		"targets: [{name: a, redfish: x, allow: [reboot]}]",
	} {
		if _, err := Load(write(t, bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestMissingDefaultFileIsEmpty(t *testing.T) {
	t.Setenv("RACKHANDS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	f, err := Load("")
	if err != nil || len(f.Targets) != 0 {
		t.Fatalf("%v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("explicit missing file must error")
	}
}

func TestTLSFingerprintConfig(t *testing.T) {
	fp := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	colon := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	p := write(t, "defaults:\n  tls_fingerprint: \""+colon+"\"\ntargets:\n  - {name: a, redfish: \"https://10.0.0.1\"}\n  - {name: b, redfish: \"https://10.0.0.2\", tls_fingerprint: \"sha256:"+fp+"\"}\n")
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := f.Find("a")
	if a.TLSFingerprint != fp {
		t.Fatalf("defaults fingerprint not normalized: %q", a.TLSFingerprint)
	}
	b, _ := f.Find("b")
	if b.TLSFingerprint != fp {
		t.Fatalf("override not normalized: %q", b.TLSFingerprint)
	}
	bad := write(t, `targets: [{name: x, redfish: "https://10.0.0.1", tls_fingerprint: "deadbeef"}]`)
	if _, err := Load(bad); err == nil {
		t.Fatal("short fingerprint accepted")
	}
}

func TestWaitTimeout(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.yaml")
	_ = os.WriteFile(p, []byte("defaults: {wait_timeout: 45m}\ntargets:\n  - {name: a, redfish: x}\n  - {name: b, redfish: y, wait_timeout: 2h}\n  - {name: c, redfish: z, wait_timeout: \"\"}\n"), 0o600)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := f.Find("a"); a.WaitTimeoutDur() != 45*time.Minute {
		t.Fatal(a.WaitTimeoutDur())
	}
	if b, _ := f.Find("b"); b.WaitTimeoutDur() != 2*time.Hour {
		t.Fatal(b.WaitTimeoutDur())
	}
	if (&Target{}).WaitTimeoutDur() != DefaultWaitTimeout {
		t.Fatal("default")
	}
	_ = os.WriteFile(p, []byte("targets:\n  - {name: a, redfish: x, wait_timeout: soon}\n"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "wait_timeout") {
		t.Fatalf("bad duration accepted: %v", err)
	}
}
