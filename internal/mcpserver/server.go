// Package mcpserver exposes rackhands as Model Context Protocol tools.
// Read tools are always present. Dangerous tools are only registered when
// their class was enabled with --allow, and each one is two-step: the first
// call returns a plan and a confirm_token, the second call (same arguments
// plus the token) executes it.
package mcpserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Blackman99/rackhands/internal/app"
	"github.com/Blackman99/rackhands/internal/bmc"
)

// Fleet selection input.
type FleetIn struct {
	Targets []string `json:"targets,omitempty" jsonschema:"target names, \"all\" (default) or \"tag:<tag>\""`
}

// One target input.
type OneIn struct {
	Target string `json:"target" jsonschema:"target name from list_targets"`
}

type SensorsIn struct {
	Target       string `json:"target" jsonschema:"target name from list_targets"`
	Kind         string `json:"kind,omitempty" jsonschema:"only this kind: temperature, fan, voltage, psu, power"`
	OnlyProblems bool   `json:"only_problems,omitempty" jsonschema:"only sensors whose health is not OK"`
}

type EventsIn struct {
	Target      string `json:"target" jsonschema:"target name from list_targets"`
	Limit       int    `json:"limit,omitempty" jsonschema:"newest N entries (default 30)"`
	AllLogs     bool   `json:"all_logs,omitempty" jsonschema:"include audit/lifecycle logs, not just the hardware event log"`
	MinSeverity string `json:"min_severity,omitempty" jsonschema:"OK, Warning or Critical (default OK = everything)"`
}

type confirmIn struct {
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"token from the plan step; only pass it after the human approved that plan"`
}

type PowerIn struct {
	Target string `json:"target" jsonschema:"one target name"`
	Action string `json:"action" jsonschema:"on, off (graceful), force-off, restart (graceful), force-restart, cycle or nmi"`
	confirmIn
}

type BMCResetIn struct {
	Target string `json:"target" jsonschema:"one target name"`
	confirmIn
}

type BootIn struct {
	Target     string `json:"target" jsonschema:"one target name"`
	BootTarget string `json:"boot_target" jsonschema:"Pxe, Hdd, Cd, Usb, BiosSetup, UefiShell, UefiHttp, Diags or None to clear"`
	Persistent bool   `json:"persistent,omitempty" jsonschema:"keep for every boot instead of next boot only"`
	confirmIn
}

type FirmwareIn struct {
	Target      string `json:"target" jsonschema:"one target name"`
	ImageURI    string `json:"image_uri" jsonschema:"http(s)/nfs/cifs URL of the firmware package that the BMC can reach"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"after the confirmed call, follow the BMC task for up to this many seconds (0 = return at once; max 900). Not part of the confirmed request."`
	confirmIn
}

type TaskIn struct {
	Target      string `json:"target" jsonschema:"target name from list_targets"`
	TaskID      string `json:"task_id" jsonschema:"id from list_tasks (e.g. JID_123456789012, 7, ilo-flash) or a task / task monitor URI"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"poll until the task ends or this many seconds pass (0 = one snapshot; max 900). Keep it under your client's tool timeout and call again to keep watching."`
}

type VMediaIn struct {
	Target string `json:"target" jsonschema:"one target name"`
	Action string `json:"action" jsonschema:"insert or eject"`
	Image  string `json:"image,omitempty" jsonschema:"ISO URL for insert"`
	confirmIn
}

func ro(title string) *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &f}
}

func danger(title string) *mcp.ToolAnnotations {
	t, f := true, false
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &t, OpenWorldHint: &f}
}

func fleet(a *app.App, fn func(context.Context, *bmc.Conn) (any, bmc.Via, error)) mcp.ToolHandlerFor[FleetIn, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in FleetIn) (*mcp.CallToolResult, any, error) {
		res, err := a.Fleet(ctx, in.Targets, fn)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"results": res}, nil
	}
}

func single(ctx context.Context, a *app.App, target string, fn func(context.Context, *bmc.Conn) (any, bmc.Via, error)) (*mcp.CallToolResult, any, error) {
	if target == "" {
		return nil, nil, fmt.Errorf("target is required")
	}
	res, err := a.Fleet(ctx, []string{target}, fn)
	if err != nil {
		return nil, nil, err
	}
	r := res[0]
	if r.Error != "" {
		return nil, nil, fmt.Errorf("%s (via %s): %s", r.Target, r.Via, r.Error)
	}
	return nil, r, nil
}

// New builds the MCP server.
func New(a *app.App, version string) *mcp.Server {
	instructions := "Out-of-band server management over Redfish (Dell iDRAC, HPE iLO, Lenovo XCC, Supermicro, any DMTF BMC) with IPMI fallback. " +
		"Read tools are safe. Dangerous tools exist only if the operator enabled them and always need two calls: the first returns a plan and confirm_token; " +
		"show the plan to the human and call again with the token only after explicit approval. Never invent or reuse tokens."
	s := mcp.NewServer(&mcp.Implementation{Name: "rackhands", Version: version}, &mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(s, &mcp.Tool{Name: "list_targets", Description: "List configured BMCs, their addresses, tags and which dangerous operation classes are enabled for each.", Annotations: ro("List BMC targets")},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, map[string]any{"targets": a.Targets()}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_inventory", Description: "Vendor, model, serial, BIOS and BMC firmware, CPU/memory summary and power state for one or many servers, plus the effective privilege of the BMC account rackhands logs in with (account.role, account.can_control) and a warning when a target with no dangerous classes enabled uses an account that could power it off.", Annotations: ro("Server inventory")},
		fleet(a, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
			return bmc.InventoryWithAccount(ctx, c)
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "get_health", Description: "Overall health (OK/Warning/Critical) and every component that is not OK, using vendor rollups (iDRAC, iLO) where available.", Annotations: ro("Server health")},
		fleet(a, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
			return bmc.Call(ctx, c, func(b bmc.Backend) (*bmc.HealthReport, error) { return b.Health(ctx) })
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "get_power_state", Description: "Power state (On/Off) of one or many servers.", Annotations: ro("Power state")},
		fleet(a, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
			st, via, err := bmc.Call(ctx, c, func(b bmc.Backend) (string, error) { return b.PowerState(ctx) })
			return map[string]string{"power_state": st}, via, err
		}))
	mcp.AddTool(s, &mcp.Tool{Name: "get_sensors", Description: "Temperatures, fans, voltages, power supplies and power draw with thresholds and health.", Annotations: ro("Sensors")},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SensorsIn) (*mcp.CallToolResult, any, error) {
			return single(ctx, a, in.Target, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
				ss, via, err := bmc.Call(ctx, c, func(b bmc.Backend) ([]bmc.Sensor, error) { return b.Sensors(ctx) })
				out := []bmc.Sensor{}
				for _, x := range ss {
					if in.Kind != "" && !strings.EqualFold(x.Kind, in.Kind) {
						continue
					}
					if in.OnlyProblems && (x.Health == "" || x.Health == bmc.OK) {
						continue
					}
					out = append(out, x)
				}
				return map[string]any{"sensors": out}, via, err
			})
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_event_log", Description: "Newest hardware event log entries (SEL, iLO IML, XCC platform log …), normalised severity.", Annotations: ro("Event log")},
		func(ctx context.Context, _ *mcp.CallToolRequest, in EventsIn) (*mcp.CallToolResult, any, error) {
			if in.Limit <= 0 {
				in.Limit = 30
			}
			return single(ctx, a, in.Target, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
				evs, via, err := bmc.Call(ctx, c, func(b bmc.Backend) ([]bmc.Event, error) { return b.Events(ctx, 2000, in.AllLogs) })
				out := []bmc.Event{}
				min := bmc.Rank(in.MinSeverity)
				for _, e := range evs {
					if bmc.Rank(e.Severity) >= min {
						out = append(out, e)
					}
					if len(out) >= in.Limit {
						break
					}
				}
				return map[string]any{"events": out}, via, err
			})
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_firmware_inventory", Description: "Installed firmware versions (BIOS, BMC, NICs, RAID, drives, PSUs).", Annotations: ro("Firmware versions")},
		func(ctx context.Context, _ *mcp.CallToolRequest, in OneIn) (*mcp.CallToolResult, any, error) {
			return single(ctx, a, in.Target, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
				fw, via, err := bmc.Call(ctx, c, func(b bmc.Backend) ([]bmc.Firmware, error) { return b.Firmware(ctx) })
				return map[string]any{"firmware": fw}, via, err
			})
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_boot_config", Description: "Boot order, one-time/persistent boot override and allowed override targets.", Annotations: ro("Boot configuration")},
		func(ctx context.Context, _ *mcp.CallToolRequest, in OneIn) (*mcp.CallToolResult, any, error) {
			return single(ctx, a, in.Target, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
				return bmc.Call(ctx, c, func(b bmc.Backend) (*bmc.Boot, error) { return b.Boot(ctx) })
			})
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_tasks", Description: "BMC tasks and jobs on one server: Redfish TaskService tasks, the Dell iDRAC job queue, the HPE iLO 5 flash state. Unfinished first. Read-only.", Annotations: ro("List BMC tasks")},
		func(ctx context.Context, _ *mcp.CallToolRequest, in OneIn) (*mcp.CallToolResult, any, error) {
			return single(ctx, a, in.Target, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
				ts, via, err := bmc.Call(ctx, c, func(b bmc.Backend) ([]bmc.Task, error) { return b.Tasks(ctx) })
				return map[string]any{"tasks": ts}, via, err
			})
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_task", Description: "One BMC task or job (state, phase, percent, messages), e.g. the task a firmware update started. With wait_seconds, polls until it completes, fails, is staged for the next reboot (phase scheduled), or the time is up. Read-only: needs no dangerous class.", Annotations: ro("BMC task")},
		func(ctx context.Context, req *mcp.CallToolRequest, in TaskIn) (*mcp.CallToolResult, any, error) {
			if in.TaskID == "" {
				return nil, nil, fmt.Errorf("task_id is required (see list_tasks)")
			}
			if in.WaitSeconds <= 0 {
				return single(ctx, a, in.Target, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
					t, via, err := bmc.Call(ctx, c, func(b bmc.Backend) (*bmc.Task, error) { return b.Task(ctx, in.TaskID) })
					return map[string]any{"task": t}, via, err
				})
			}
			out, err := a.WatchTask(ctx, in.Target, in.TaskID, watchOpts(ctx, req, in.WaitSeconds))
			if err != nil {
				return nil, nil, err
			}
			return nil, outcome(out), nil
		})

	p := a.Policy
	if p.Enabled("power") {
		mcp.AddTool(s, &mcp.Tool{Name: "power_action", Description: "DANGEROUS. Power on/off/restart/cycle a server. Two-step: call without confirm_token to get a plan, show it to the human, call again with the token after approval.", Annotations: danger("Power action")},
			func(ctx context.Context, _ *mcp.CallToolRequest, in PowerIn) (*mcp.CallToolResult, any, error) {
				return twoStep(ctx, a, app.Danger{Target: in.Target, Class: "power", Action: in.Action}, in.ConfirmToken)
			})
	}
	if p.Enabled("bmc-reset") {
		mcp.AddTool(s, &mcp.Tool{Name: "reset_bmc", Description: "DANGEROUS. Restart the BMC itself (host keeps running, BMC unreachable for minutes). Two-step confirm.", Annotations: danger("Reset BMC")},
			func(ctx context.Context, _ *mcp.CallToolRequest, in BMCResetIn) (*mcp.CallToolResult, any, error) {
				return twoStep(ctx, a, app.Danger{Target: in.Target, Class: "bmc-reset", Action: "reset"}, in.ConfirmToken)
			})
	}
	if p.Enabled("boot") {
		mcp.AddTool(s, &mcp.Tool{Name: "set_boot_override", Description: "DANGEROUS. Set a one-time (or persistent) boot device such as Pxe or Cd. Two-step confirm.", Annotations: danger("Boot override")},
			func(ctx context.Context, _ *mcp.CallToolRequest, in BootIn) (*mcp.CallToolResult, any, error) {
				return twoStep(ctx, a, app.Danger{Target: in.Target, Class: "boot", Action: "override",
					Args: map[string]string{"boot_target": in.BootTarget, "persistent": strconv.FormatBool(in.Persistent)}}, in.ConfirmToken)
			})
	}
	if p.Enabled("firmware") {
		mcp.AddTool(s, &mcp.Tool{Name: "update_firmware", Description: "DANGEROUS. Push a firmware package to the BMC via Redfish SimpleUpdate. Two-step confirm.", Annotations: danger("Firmware update")},
			func(ctx context.Context, req *mcp.CallToolRequest, in FirmwareIn) (*mcp.CallToolResult, any, error) {
				d := app.Danger{Target: in.Target, Class: "firmware", Action: "update", Args: map[string]string{"image_uri": in.ImageURI}}
				if in.ConfirmToken == "" || in.WaitSeconds <= 0 {
					res, out, err := twoStep(ctx, a, d, in.ConfirmToken)
					if m, ok := out.(map[string]any); ok && err == nil && m["status"] == "done" {
						if ref, _ := m["task_ref"].(string); ref != "" {
							m["next"] = fmt.Sprintf("The BMC is applying it. Follow it read-only with get_task {target: %q, task_id: %q, wait_seconds: 60}.", in.Target, bmc.ShortRef(ref))
						}
					}
					return res, out, err
				}
				w := watchOpts(ctx, req, in.WaitSeconds)
				p, res, follow, err := a.ConfirmFollow(ctx, "mcp", d, in.ConfirmToken, &w)
				if err != nil {
					return nil, nil, err
				}
				return nil, map[string]any{"status": "done", "summary": p.Plan.Summary, "via": p.Via, "result": res, "task": outcome(follow)}, nil
			})
	}
	if p.Enabled("virtual-media") {
		mcp.AddTool(s, &mcp.Tool{Name: "virtual_media", Description: "DANGEROUS. Insert an ISO URL as virtual CD, or eject it. Two-step confirm.", Annotations: danger("Virtual media")},
			func(ctx context.Context, _ *mcp.CallToolRequest, in VMediaIn) (*mcp.CallToolResult, any, error) {
				if in.Action != "insert" && in.Action != "eject" {
					return nil, nil, fmt.Errorf("action must be insert or eject")
				}
				return twoStep(ctx, a, app.Danger{Target: in.Target, Class: "virtual-media", Action: in.Action, Args: map[string]string{"image": in.Image}}, in.ConfirmToken)
			})
	}
	return s
}

func twoStep(ctx context.Context, a *app.App, d app.Danger, token string) (*mcp.CallToolResult, any, error) {
	if token == "" {
		p, err := a.Plan(ctx, "mcp", d, true)
		if err != nil {
			return nil, nil, err
		}
		defer p.Close()
		return nil, map[string]any{
			"status":        "confirmation_required",
			"plan":          p.Plan,
			"via":           p.Via,
			"confirm_token": p.Token,
			"expires":       p.Expires.UTC().Format("2006-01-02T15:04:05Z"),
			"next":          "Show this plan to the human verbatim. Only if they explicitly approve, call this tool again with the same arguments plus confirm_token. Nothing has been changed yet.",
		}, nil
	}
	p, res, err := a.Confirm(ctx, "mcp", d, token)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{"status": "done", "summary": p.Plan.Summary, "via": p.Via, "result": res}
	if ref, _ := p.Plan.Task(); ref != "" {
		out["task_ref"] = ref
	}
	return nil, out, nil
}

// maxWait caps one MCP call that watches a task.
const maxWait = 900 * time.Second

// Poll cadence while watching (tests shorten it).
var pollEvery, pollMax = 2 * time.Second, 15 * time.Second

// watchOpts bounds a watch to wait_seconds and reports progress to clients
// that asked for it (the progress value is the poll count, so it always
// increases; the message carries state and percent).
func watchOpts(ctx context.Context, req *mcp.CallToolRequest, waitSeconds int) bmc.WatchOptions {
	d := min(time.Duration(waitSeconds)*time.Second, maxWait)
	w := bmc.WatchOptions{Timeout: d, Every: pollEvery, MaxEvery: pollMax}
	if req == nil || req.Params == nil || req.Session == nil {
		return w
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return w
	}
	w.OnUpdate = func(u bmc.TaskUpdate) {
		msg := "BMC not answering, retrying"
		if u.Err == nil && u.Task != nil {
			msg = u.Task.State
			if u.Task.Percent != nil {
				msg += fmt.Sprintf(" %d%%", *u.Task.Percent)
			}
			if u.Task.Message != "" {
				msg += ": " + u.Task.Message
			}
		}
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: float64(u.Polls), Message: msg})
	}
	return w
}

// outcome adds the next step an agent should take to a watch result.
func outcome(o *bmc.Outcome) map[string]any {
	if o == nil {
		return nil
	}
	m := map[string]any{"target": o.Target, "task_ref": o.Ref, "verdict": o.Verdict, "task": o.Task, "elapsed_s": o.Elapsed, "polls": o.Polls}
	if o.Note != "" {
		m["note"] = o.Note
	}
	switch o.Verdict {
	case bmc.VerdictTimeout:
		m["next"] = fmt.Sprintf("Still running on the BMC; nothing is wrong yet. Call get_task {target: %q, task_id: %q, wait_seconds: 60} again to keep watching.", o.Target, bmc.ShortRef(o.Ref))
	case bmc.PhaseScheduled:
		m["next"] = "Staged: the BMC applies it at the next host reboot. Rebooting is a separate power action that the human must approve."
	case bmc.PhaseFailed, bmc.PhaseCancelled, bmc.PhaseAttention, bmc.VerdictVanished:
		m["next"] = "Tell the human the task did not complete and show them the message; do not retry the update on your own."
	}
	return m
}
