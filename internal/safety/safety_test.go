package safety

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// Two identical plans issued in the same second must get distinct tokens,
// each redeemable exactly once (regression: the token used to be a pure
// function of the request and the expiry second).
func TestTokensSameSecondIdenticalPlans(t *testing.T) {
	tk := NewTokens(time.Minute)
	now := time.Unix(1_800_000_000, 0)
	tk.now = func() time.Time { return now }
	op := Op{Target: "db-01", Class: "power", Action: "force-restart"}
	a, expA := tk.Issue(op)
	b, expB := tk.Issue(op)
	if a == b {
		t.Fatalf("identical same-second plans got the same token %q", a)
	}
	if !expA.Equal(expB) {
		t.Fatal("test precondition: both tokens must share the expiry second")
	}
	seen := map[string]bool{a: true, b: true}
	for i := 0; i < 1000; i++ {
		tok, _ := tk.Issue(op)
		if seen[tok] {
			t.Fatalf("duplicate token after %d issues", i)
		}
		seen[tok] = true
	}
	for _, tok := range []string{b, a} { // order must not matter
		if err := tk.Redeem(op, tok); err != nil {
			t.Fatalf("redeem %q: %v", tok, err)
		}
	}
	for _, tok := range []string{a, b} {
		if err := tk.Redeem(op, tok); err == nil || !strings.Contains(err.Error(), "already used") {
			t.Fatalf("replay of %q: %v", tok, err)
		}
	}
}

func TestTokenSingleUseAndBinding(t *testing.T) {
	tk := NewTokens(time.Minute)
	op := Op{Target: "db-01", Class: "boot", Action: "override", Args: map[string]string{"boot_target": "Pxe", "persistent": "false"}}
	tok, _ := tk.Issue(op)
	parts := strings.Split(tok, "-")
	if len(parts) != 4 || parts[0] != "rh2" {
		t.Fatalf("format: %q", tok)
	}
	mismatches := []Op{
		{Target: "db-02", Class: op.Class, Action: op.Action, Args: op.Args},
		{Target: op.Target, Class: "power", Action: op.Action, Args: op.Args},
		{Target: op.Target, Class: op.Class, Action: "clear", Args: op.Args},
		{Target: op.Target, Class: op.Class, Action: op.Action, Args: map[string]string{"boot_target": "Cd", "persistent": "false"}},
		{Target: op.Target, Class: op.Class, Action: op.Action, Args: map[string]string{"boot_target": "Pxe", "persistent": "true"}},
		{Target: op.Target, Class: op.Class, Action: op.Action, Args: map[string]string{"boot_target": "Pxe"}},
	}
	for _, m := range mismatches {
		if err := tk.Redeem(m, tok); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("mismatch %+v: %v", m, err)
		}
	}
	// A mismatch must not burn the token for the real request.
	if err := tk.Redeem(op, tok); err != nil {
		t.Fatal(err)
	}
	if err := tk.Redeem(op, tok); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("second use: %v", err)
	}
}

func TestTokenTamperAndForgery(t *testing.T) {
	tk := NewTokens(time.Minute)
	op := Op{Target: "db-01", Class: "power", Action: "force-off"}
	tok, _ := tk.Issue(op)
	other, _ := tk.Issue(op)
	p := strings.Split(tok, "-")
	q := strings.Split(other, "-")
	exp, _ := strconv.ParseInt(p[1], 10, 64)
	flip := func(s string) string {
		c := byte('0')
		if s[0] == '0' {
			c = '1'
		}
		return string(c) + s[1:]
	}
	bad := map[string]string{
		"nonce swapped from another token": strings.Join([]string{p[0], p[1], q[2], p[3]}, "-"),
		"nonce altered":                    strings.Join([]string{p[0], p[1], flip(p[2]), p[3]}, "-"),
		"expiry extended":                  strings.Join([]string{p[0], strconv.FormatInt(exp+3600, 10), p[2], p[3]}, "-"),
		"mac altered":                      strings.Join([]string{p[0], p[1], p[2], flip(p[3])}, "-"),
		"uppercase nonce":                  strings.Join([]string{p[0], p[1], strings.ToUpper(p[2]), p[3]}, "-"),
		"short nonce":                      strings.Join([]string{p[0], p[1], p[2][:8], p[3]}, "-"),
		"missing nonce (rh1 format)":       strings.Join([]string{"rh1", p[1], p[3]}, "-"),
		"old prefix":                       strings.Join([]string{"rh1", p[1], p[2], p[3]}, "-"),
		"empty":                            "",
		"extra field":                      tok + "-x",
	}
	for name, b := range bad {
		if err := tk.Redeem(op, b); err == nil {
			t.Fatalf("%s: accepted %q", name, b)
		}
	}
	// None of the rejected variants burned the genuine tokens.
	for _, good := range []string{tok, other} {
		if err := tk.Redeem(op, good); err != nil {
			t.Fatal(err)
		}
	}
}

// A used token must stay burned for as long as it could still pass the
// expiry check (whole seconds), even if another redeem prunes the used-set
// a fraction of a second after the token's expiry.
func TestTokenNoReplayAfterSubSecondPrune(t *testing.T) {
	tk := NewTokens(time.Minute)
	base := time.Unix(1_800_000_000, 0)
	now := base
	tk.now = func() time.Time { return now }
	op := Op{Target: "db-01", Class: "power", Action: "force-off"}
	tok, exp := tk.Issue(op)
	if err := tk.Redeem(op, tok); err != nil {
		t.Fatal(err)
	}
	// 100ms past expiry, still inside the expiry second: an unrelated redeem
	// runs the pruning loop.
	now = exp.Add(100 * time.Millisecond)
	other, _ := tk.Issue(op)
	if err := tk.Redeem(op, other); err != nil {
		t.Fatal(err)
	}
	now = exp.Add(300 * time.Millisecond)
	if err := tk.Redeem(op, tok); err == nil {
		t.Fatal("used token replayed after a sub-second prune")
	}
	// After the expiry second the token is rejected as expired.
	now = exp.Add(1100 * time.Millisecond)
	if err := tk.Redeem(op, tok); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("after expiry: %v", err)
	}
}
