package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackman99/rackhands/internal/bmc"
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

func TestFollowAuditsTheTaskOutcome(t *testing.T) {
	ctx := context.Background()
	a, dell, audit := newApp(t, "firmware")
	a.Cfg.Targets[0].Allow = []string{"firmware"}
	d := Danger{Target: "db-01", Class: "firmware", Action: "update", Args: map[string]string{"image_uri": "https://repo.example/iDRAC.EXE"}}
	p, err := a.Plan(ctx, "test", d, true)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	w := bmc.WatchOptions{Every: time.Millisecond, MaxEvery: time.Millisecond}
	_, res, out, err := a.ConfirmFollow(ctx, "test", d, p.Token, &w)
	if err != nil || !strings.Contains(res, "JID_001") || out.Verdict != bmc.PhaseCompleted {
		t.Fatalf("%v %q %+v", err, res, out)
	}
	b, _ := os.ReadFile(audit)
	last := strings.TrimSpace(string(b))
	last = last[strings.LastIndex(last, "\n")+1:]
	for _, want := range []string{`"phase":"task"`, `"verdict":"completed"`, `"task_state":"Completed"`, `"watched_s":`, `"task":"/redfish/v1/TaskService/Tasks/JID_001"`, `"class":"firmware"`} {
		if !strings.Contains(last, want) {
			t.Fatalf("audit outcome missing %s:\n%s", want, last)
		}
	}

	// A BMC that accepts the update but returns no task: say so, audit it.
	dell.TaskLocation = ""
	d.Args = map[string]string{"image_uri": "https://repo.example/BIOS.EXE"}
	p, _ = a.Plan(ctx, "test", d, true)
	p.Close()
	_, _, out, err = a.ConfirmFollow(ctx, "test", d, p.Token, &w)
	if err != nil || out.Verdict != bmc.VerdictNoTask || !out.Ok() {
		t.Fatalf("%v %+v", err, out)
	}
	if b, _ := os.ReadFile(audit); !strings.Contains(string(b), `"verdict":"no-task"`) {
		t.Fatal("no-task outcome not audited")
	}
}

func TestWatchTaskIsReadOnly(t *testing.T) {
	a, dell, audit := newApp(t) // no --allow at all
	out, err := a.WatchTask(context.Background(), "db-02", "JID_002", bmc.WatchOptions{Every: time.Millisecond})
	if err != nil || out.Verdict != bmc.PhaseFailed {
		t.Fatalf("%v %+v", err, out)
	}
	if dell.Count() != 0 {
		t.Fatal("watching a task must not write to the BMC")
	}
	if _, err := os.Stat(audit); err == nil {
		t.Fatal("reads are not audited")
	}
	if _, err := a.WatchTask(context.Background(), "tag:db", "JID_002", bmc.WatchOptions{}); err == nil {
		t.Fatal("watch fanned out")
	}
}
