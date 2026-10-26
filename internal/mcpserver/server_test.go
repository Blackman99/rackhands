package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Blackman99/rackhands/internal/app"
	"github.com/Blackman99/rackhands/internal/config"
	"github.com/Blackman99/rackhands/internal/safety"
	"github.com/Blackman99/rackhands/internal/testbmc"
)

func setup(t *testing.T, allow ...string) (*mcp.ClientSession, *testbmc.Server, string) {
	t.Helper()
	dell := testbmc.Start(t, "dell")
	hpe := testbmc.Start(t, "hpe")
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	a := &app.App{
		Cfg: &config.File{Targets: []config.Target{
			{Name: "db-01", Redfish: dell.URL, Allow: []string{"power", "firmware"}, Tags: []string{"db"}},
			{Name: "web-07", Redfish: hpe.URL},
		}},
		Policy: safety.Policy{Runtime: safety.ParseAllow(allow)},
		Tokens: safety.NewTokens(time.Minute),
		Audit:  safety.NewAudit(audit),
	}
	srv := New(a, "test")
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, dell, audit
}

func tools(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		m[tl.Name] = tl
	}
	return m
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		var msg string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg += tc.Text
			}
		}
		return map[string]any{"error": msg}, false
	}
	b, _ := json.Marshal(res.StructuredContent)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, true
}

func TestReadOnlyByDefault(t *testing.T) {
	cs, _, _ := setup(t)
	ts := tools(t, cs)
	for _, n := range []string{"power_action", "reset_bmc", "set_boot_override", "update_firmware", "virtual_media"} {
		if _, ok := ts[n]; ok {
			t.Errorf("%s registered without --allow", n)
		}
	}
	for _, n := range []string{"list_targets", "get_inventory", "get_health", "get_sensors", "get_event_log", "get_power_state", "get_firmware_inventory", "get_boot_config", "list_tasks", "get_task", "get_storage"} {
		tl, ok := ts[n]
		if !ok {
			t.Fatalf("missing %s", n)
		}
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s not annotated read-only", n)
		}
	}
}

func TestReadTools(t *testing.T) {
	cs, _, _ := setup(t)
	out, ok := call(t, cs, "get_health", map[string]any{"targets": []string{"all"}})
	if !ok {
		t.Fatal(out)
	}
	s, _ := json.Marshal(out)
	if !strings.Contains(string(s), `"overall":"Critical"`) || !strings.Contains(string(s), "web-07") {
		t.Fatalf("%s", s)
	}
	out, ok = call(t, cs, "get_event_log", map[string]any{"target": "web-07", "min_severity": "Warning", "limit": 5})
	s, _ = json.Marshal(out)
	if !ok || !strings.Contains(string(s), "Fan Failure") || strings.Contains(string(s), "power restored") {
		t.Fatalf("%s", s)
	}
	out, ok = call(t, cs, "get_sensors", map[string]any{"target": "db-01", "only_problems": true})
	s, _ = json.Marshal(out)
	if !ok || !strings.Contains(string(s), "PS2") || strings.Contains(string(s), "CPU1 Temp") {
		t.Fatalf("%s", s)
	}
	if out, ok := call(t, cs, "get_inventory", map[string]any{"targets": []string{"nope"}}); ok {
		t.Fatalf("unknown target should fail: %v", out)
	}
}

func TestTwoStepPower(t *testing.T) {
	cs, dell, audit := setup(t, "power")
	if _, ok := tools(t, cs)["power_action"]; !ok {
		t.Fatal("power_action not registered with --allow power")
	}
	if _, ok := tools(t, cs)["reset_bmc"]; ok {
		t.Fatal("reset_bmc registered without its class")
	}
	plan, ok := call(t, cs, "power_action", map[string]any{"target": "db-01", "action": "force-off"})
	if !ok || plan["status"] != "confirmation_required" {
		t.Fatalf("%v", plan)
	}
	if dell.Count() != 0 {
		t.Fatal("plan step wrote to the BMC")
	}
	tok := plan["confirm_token"].(string)
	// Same token, different action: refused.
	if out, ok := call(t, cs, "power_action", map[string]any{"target": "db-01", "action": "on", "confirm_token": tok}); ok {
		t.Fatalf("token reused for another action: %v", out)
	}
	done, ok := call(t, cs, "power_action", map[string]any{"target": "db-01", "action": "force-off", "confirm_token": tok})
	if !ok || done["status"] != "done" {
		t.Fatalf("%v", done)
	}
	w, _ := dell.Last()
	if w.Body["ResetType"] != "ForceOff" {
		t.Fatalf("%+v", w)
	}
	if _, ok := call(t, cs, "power_action", map[string]any{"target": "db-01", "action": "force-off", "confirm_token": tok}); ok {
		t.Fatal("token replay accepted")
	}
	// web-07 has no allow: power in the config: second key missing.
	if out, ok := call(t, cs, "power_action", map[string]any{"target": "web-07", "action": "on"}); ok || !strings.Contains(out["error"].(string), "allow") {
		t.Fatalf("%v", out)
	}
	if out, ok := call(t, cs, "power_action", map[string]any{"target": "all", "action": "on"}); ok {
		t.Fatalf("fan-out accepted: %v", out)
	}
	b, _ := readFile(audit)
	for _, phase := range []string{`"planned"`, `"executed"`, `"token-rejected"`, `"refused"`} {
		if !strings.Contains(b, phase) {
			t.Errorf("audit missing %s", phase)
		}
	}
}

func fastPolls(t *testing.T) {
	e, m := pollEvery, pollMax
	pollEvery, pollMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { pollEvery, pollMax = e, m })
}

func TestTasksAreReadOnly(t *testing.T) {
	fastPolls(t)
	cs, dell, _ := setup(t) // nothing allowed
	out, ok := call(t, cs, "list_tasks", map[string]any{"target": "db-01"})
	s, _ := json.Marshal(out)
	if !ok || !strings.Contains(string(s), `"id":"JID_003"`) || !strings.Contains(string(s), `"phase":"scheduled"`) {
		t.Fatalf("%s", s)
	}
	out, ok = call(t, cs, "get_task", map[string]any{"target": "db-01", "task_id": "JID_002"})
	s, _ = json.Marshal(out)
	if !ok || !strings.Contains(string(s), `"phase":"failed"`) || !strings.Contains(string(s), "Update Package signature") {
		t.Fatalf("%s", s)
	}
	out, ok = call(t, cs, "get_task", map[string]any{"target": "web-07", "task_id": "ilo-flash", "wait_seconds": 30})
	if !ok || out["verdict"] != "completed" {
		t.Fatalf("%v", out)
	}
	if out, ok := call(t, cs, "get_task", map[string]any{"target": "db-01"}); ok {
		t.Fatalf("task_id required: %v", out)
	}
	if dell.Count() != 0 {
		t.Fatal("task tools wrote to the BMC")
	}
}

func TestGetTaskProgressNotifications(t *testing.T) {
	fastPolls(t)
	dell := testbmc.Start(t, "dell")
	a := &app.App{Cfg: &config.File{Targets: []config.Target{{Name: "db-01", Redfish: dell.URL}}}, Tokens: safety.NewTokens(time.Minute)}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := New(a, "test").Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var msgs []string
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			msgs = append(msgs, r.Params.Message)
			mu.Unlock()
		}}).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	params := &mcp.CallToolParams{Name: "get_task", Arguments: map[string]any{"target": "db-01", "task_id": "/redfish/v1/TaskService/Tasks/JID_001", "wait_seconds": 30}}
	params.SetProgressToken("p1")
	res, err := cs.CallTool(ctx, params)
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(msgs) < 3 || !strings.Contains(strings.Join(msgs, "|"), "Running 40%: Installing the iDRAC firmware package.") {
		t.Fatalf("progress: %q", msgs)
	}
}

func TestUpdateFirmwareWait(t *testing.T) {
	fastPolls(t)
	cs, dell, audit := setup(t, "firmware")
	args := map[string]any{"target": "db-01", "image_uri": "https://repo.example/iDRAC.EXE", "wait_seconds": 60}
	plan, ok := call(t, cs, "update_firmware", args)
	if !ok || plan["status"] != "confirmation_required" || dell.Count() != 0 {
		t.Fatalf("wait_seconds must not skip the plan step: %v", plan)
	}
	args["confirm_token"] = plan["confirm_token"]
	args["wait_seconds"] = 30 // waiting is not part of the confirmed request
	done, ok := call(t, cs, "update_firmware", args)
	s, _ := json.Marshal(done)
	if !ok || done["status"] != "done" || !strings.Contains(string(s), `"verdict":"completed"`) || dell.Count() != 1 {
		t.Fatalf("%s", s)
	}
	if b, _ := readFile(audit); !strings.Contains(b, `"phase":"task"`) || !strings.Contains(b, `"verdict":"completed"`) {
		t.Fatalf("outcome not audited:\n%s", b)
	}
	// Without wait_seconds: return at once and say how to follow it.
	args = map[string]any{"target": "db-01", "image_uri": "https://repo.example/BIOS.EXE"}
	plan, _ = call(t, cs, "update_firmware", args)
	args["confirm_token"] = plan["confirm_token"]
	done, ok = call(t, cs, "update_firmware", args)
	if next, _ := done["next"].(string); !ok || !strings.Contains(next, `get_task {target: "db-01", task_id: "JID_001"`) {
		t.Fatalf("%v", done)
	}
}

// Regression: two identical plans back to back (same second) used to get the
// same token, so approving the second one failed with "already used".
func TestTwoIdenticalPlansSameSecond(t *testing.T) {
	cs, dell, _ := setup(t, "power")
	args := map[string]any{"target": "db-01", "action": "force-restart"}
	p1, ok1 := call(t, cs, "power_action", args)
	p2, ok2 := call(t, cs, "power_action", args)
	if !ok1 || !ok2 {
		t.Fatalf("%v %v", p1, p2)
	}
	t1, t2 := p1["confirm_token"].(string), p2["confirm_token"].(string)
	if t1 == t2 {
		t.Fatalf("identical plans got the same token %q", t1)
	}
	for i, tok := range []string{t1, t2} {
		done, ok := call(t, cs, "power_action", map[string]any{"target": "db-01", "action": "force-restart", "confirm_token": tok})
		if !ok || done["status"] != "done" {
			t.Fatalf("plan %d: %v", i+1, done)
		}
	}
	if dell.Count() != 2 {
		t.Fatalf("want exactly 2 writes, got %d", dell.Count())
	}
	for _, tok := range []string{t1, t2} {
		if out, ok := call(t, cs, "power_action", map[string]any{"target": "db-01", "action": "force-restart", "confirm_token": tok}); ok || !strings.Contains(out["error"].(string), "already used") {
			t.Fatalf("replay: %v", out)
		}
	}
	if dell.Count() != 2 {
		t.Fatal("replay wrote to the BMC")
	}
}

func TestStorageToolIsReadOnly(t *testing.T) {
	// No --allow at all: storage is a read and must work anyway, write
	// nothing to the BMC and nothing to the audit log.
	cs, dell, audit := setup(t)
	out, ok := call(t, cs, "get_storage", map[string]any{"targets": []string{"all"}})
	if !ok {
		t.Fatal(out)
	}
	s, _ := json.Marshal(out)
	for _, w := range []string{`"name":"Physical Disk 0:1:2"`, `"raid_status":"Failed"`, `"name":"pgdata"`, `"failure_predicted":true`, `"life_left_percent":6`} {
		if !strings.Contains(string(s), w) {
			t.Errorf("missing %s in %s", w, s)
		}
	}
	out, _ = call(t, cs, "get_health", map[string]any{"targets": []string{"db-01"}})
	s, _ = json.Marshal(out)
	if !strings.Contains(string(s), `"component":"drive Physical Disk 0:1:2"`) || !strings.Contains(string(s), `"storage":{`) {
		t.Fatalf("get_health lacks storage: %s", s)
	}
	if dell.Count() != 0 {
		t.Fatalf("storage wrote to the BMC: %+v", dell.Writes)
	}
	if b, err := os.ReadFile(audit); err == nil && len(b) > 0 {
		t.Fatalf("storage reads were audited: %s", b)
	}
}
