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
	Target   string `json:"target" jsonschema:"one target name"`
	ImageURI string `json:"image_uri" jsonschema:"http(s)/nfs/cifs URL of the firmware package that the BMC can reach"`
	confirmIn
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
			func(ctx context.Context, _ *mcp.CallToolRequest, in FirmwareIn) (*mcp.CallToolResult, any, error) {
				return twoStep(ctx, a, app.Danger{Target: in.Target, Class: "firmware", Action: "update", Args: map[string]string{"image_uri": in.ImageURI}}, in.ConfirmToken)
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
	return nil, map[string]any{"status": "done", "summary": p.Plan.Summary, "via": p.Via, "result": res}, nil
}
