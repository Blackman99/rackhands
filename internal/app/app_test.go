package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackman99/rackhands/internal/config"
	"github.com/Blackman99/rackhands/internal/safety"
	"github.com/Blackman99/rackhands/internal/testbmc"
)

func newApp(t *testing.T, allow ...string) (*App, *testbmc.Server, string) {
	dell := testbmc.Start(t, "dell")
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	return &App{
		Cfg: &config.File{Targets: []config.Target{
			{Name: "db-01", Redfish: dell.URL, Allow: []string{"power"}, Tags: []string{"db"}},
			{Name: "db-02", Redfish: dell.URL, Tags: []string{"db"}},
		}},
		Policy: safety.Policy{Runtime: safety.ParseAllow(allow)},
		Tokens: safety.NewTokens(time.Minute),
		Audit:  safety.NewAudit(audit),
	}, dell, audit
}

func TestPlanConfirm(t *testing.T) {
	a, dell, audit := newApp(t, "power")
	ctx := context.Background()
	d := Danger{Target: "db-01", Class: "power", Action: "force-off"}
	p, err := a.Plan(ctx, "test", d, true)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if p.Token == "" || dell.Count() != 0 {
		t.Fatalf("plan must issue a token and send nothing (writes=%d)", dell.Count())
	}
	other := d
	other.Action = "on"
	if _, _, err := a.Confirm(ctx, "test", other, p.Token); err == nil {
		t.Fatal("token accepted for a different action")
	}
	if _, _, err := a.Confirm(ctx, "test", d, "rh1-0-bogus"); err == nil {
		t.Fatal("bogus token accepted")
	}
	if _, res, err := a.Confirm(ctx, "test", d, p.Token); err != nil || !strings.Contains(res, "204") {
		t.Fatalf("%q %v", res, err)
	}
	if _, _, err := a.Confirm(ctx, "test", d, p.Token); err == nil {
		t.Fatal("token replay accepted")
	}
	b, _ := os.ReadFile(audit)
	if !strings.Contains(string(b), `"phase":"executed"`) || strings.Contains(string(b), `"via":""`) {
		t.Fatalf("audit:\n%s", b)
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	a, _, _ := newApp(t)
	if _, err := a.Plan(ctx, "t", Danger{Target: "db-01", Class: "power", Action: "on"}, false); err == nil {
		t.Fatal("no runtime key")
	}
	a, _, _ = newApp(t, "power")
	if _, err := a.Plan(ctx, "t", Danger{Target: "db-02", Class: "power", Action: "on"}, false); err == nil {
		t.Fatal("no target key")
	}
	for _, tg := range []string{"all", "tag:db", "db-*", "nope"} {
		if _, err := a.Plan(ctx, "t", Danger{Target: tg, Class: "power", Action: "on"}, false); err == nil {
			t.Fatalf("%q accepted for a dangerous op", tg)
		}
	}
}

func TestFleetSelectors(t *testing.T) {
	a, _, _ := newApp(t)
	ts, err := a.Resolve([]string{"tag:db"})
	if err != nil || len(ts) != 2 {
		t.Fatalf("%v %d", err, len(ts))
	}
	if _, err := a.Resolve([]string{"db-09"}); err == nil {
		t.Fatal("unknown target resolved")
	}
}
