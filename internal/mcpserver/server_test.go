package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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
			{Name: "db-01", Redfish: dell.URL, Allow: []string{"power"}, Tags: []string{"db"}},
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
	for _, n := range []string{"list_targets", "get_inventory", "get_health", "get_sensors", "get_event_log", "get_power_state", "get_firmware_inventory", "get_boot_config"} {
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
