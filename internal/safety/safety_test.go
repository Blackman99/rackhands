package safety

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokens(t *testing.T) {
	tk := NewTokens(time.Minute)
	op := Op{Target: "db-01", Class: "power", Action: "force-off", Args: map[string]string{"via": "redfish"}}
	tok, _ := tk.Issue(op)
	other := op
	other.Action = "on"
	if err := tk.Redeem(other, tok); err == nil {
		t.Fatal("token must be bound to the action")
	}
	other = Op{Target: "db-02", Class: "power", Action: "force-off", Args: op.Args}
	if err := tk.Redeem(other, tok); err == nil {
		t.Fatal("token must be bound to the target")
	}
	if err := tk.Redeem(op, tok); err != nil {
		t.Fatal(err)
	}
	if err := tk.Redeem(op, tok); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("reuse: %v", err)
	}
	if err := tk.Redeem(op, "rh1-1-abc"); err == nil {
		t.Fatal("forged token accepted")
	}
	if err := NewTokens(time.Minute).Redeem(op, tok); err == nil {
		t.Fatal("token from another process accepted")
	}
}

func TestTokenExpiry(t *testing.T) {
	tk := NewTokens(time.Minute)
	now := time.Now()
	tk.now = func() time.Time { return now }
	op := Op{Target: "a", Class: "boot", Action: "override", Args: map[string]string{"boot_target": "Pxe"}}
	tok, _ := tk.Issue(op)
	tk.now = func() time.Time { return now.Add(2 * time.Minute) }
	if err := tk.Redeem(op, tok); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expiry: %v", err)
	}
}

func TestPolicyNeedsBothKeys(t *testing.T) {
	p := Policy{Runtime: ParseAllow([]string{"power,boot"})}
	if err := p.Check("power", "db", []string{"power"}, true); err != nil {
		t.Fatal(err)
	}
	if err := p.Check("power", "db", nil, true); !errors.Is(err, ErrNotAllowed) {
		t.Fatal("target without allow: must refuse")
	}
	if err := p.Check("firmware", "db", []string{"all"}, true); !errors.Is(err, ErrNotAllowed) {
		t.Fatal("runtime key missing must refuse")
	}
	if err := p.Check("boot", "10.0.0.5", nil, false); err != nil {
		t.Fatal("ad-hoc target needs only the runtime key")
	}
	if (Policy{}).AnyEnabled() {
		t.Fatal("default must be read-only")
	}
}

func TestAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	a := NewAudit(path)
	if err := a.Record("cli", "planned", Op{Target: "x", Class: "power"}, map[string]any{"summary": "s"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"phase":"planned"`) {
		t.Fatal(string(b))
	}
	if NewAudit("off") != nil || NewAudit("") != nil {
		t.Fatal("off")
	}
	var nilAudit *Audit
	if nilAudit.Record("x", "y", Op{}, nil) != nil {
		t.Fatal("nil audit must be a no-op")
	}
}
