// Package app is the shared core of the CLI and the MCP server: target
// resolution, fleet fan-out, and the plan → confirm → execute flow.
package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Blackman99/rackhands/internal/bmc"
	"github.com/Blackman99/rackhands/internal/config"
	"github.com/Blackman99/rackhands/internal/safety"
)

// App holds configuration and safety state.
type App struct {
	Cfg    *config.File
	AdHoc  *config.Target // from --host/--ipmi; nil when using the config file
	Policy safety.Policy
	Tokens *safety.Tokens
	Audit  *safety.Audit
}

// Result is one target's answer in a fleet call.
type Result struct {
	Target string `json:"target"`
	Via    string `json:"via,omitempty"`
	// FallbackReason says why Redfish was skipped when Via is "ipmi (fallback)".
	FallbackReason string `json:"fallback_reason,omitempty"`
	Data           any    `json:"data,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Resolve turns a selection into config targets. Empty selection means all
// configured targets (or the ad-hoc one).
func (a *App) Resolve(sel []string) ([]*config.Target, error) {
	if a.AdHoc != nil {
		if len(sel) == 0 || (len(sel) == 1 && sel[0] == a.AdHoc.Name) {
			return []*config.Target{a.AdHoc}, nil
		}
		return nil, fmt.Errorf("with --host only the target %q exists", a.AdHoc.Name)
	}
	if len(a.Cfg.Targets) == 0 {
		return nil, fmt.Errorf("no targets: create %s or pass --host (see `rackhands help`)", a.Cfg.Path())
	}
	if len(sel) == 0 {
		sel = []string{"all"}
	}
	return a.Cfg.Select(sel)
}

// One resolves exactly one target (dangerous operations never fan out).
func (a *App) One(name string) (*config.Target, error) {
	if name == "" || name == "all" || len(name) > 4 && name[:4] == "tag:" {
		return nil, errors.New("dangerous operations take exactly one named target")
	}
	ts, err := a.Resolve([]string{name})
	if err != nil {
		return nil, err
	}
	return ts[0], nil
}

// Fleet runs fn on every selected target concurrently, in input order.
func (a *App) Fleet(ctx context.Context, sel []string, fn func(context.Context, *bmc.Conn) (any, bmc.Via, error)) ([]Result, error) {
	ts, err := a.Resolve(sel)
	if err != nil {
		return nil, err
	}
	out := make([]Result, len(ts))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Add(1)
		go func(i int, t *config.Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = a.one(ctx, t, fn)
		}(i, t)
	}
	wg.Wait()
	return out, nil
}

func (a *App) one(ctx context.Context, t *config.Target, fn func(context.Context, *bmc.Conn) (any, bmc.Via, error)) Result {
	r := Result{Target: t.Name}
	c, err := bmc.Open(t)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer c.Close(context.Background())
	tctx, cancel := context.WithTimeout(ctx, 3*t.TimeoutDur())
	defer cancel()
	data, via, err := fn(tctx, c)
	r.Via = via.Protocol
	if via.Fallback != "" {
		r.Via += " (fallback)"
		r.FallbackReason = via.Fallback
	}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Data = data
	return r
}

// Danger is a dangerous request.
type Danger struct {
	Target string
	Class  string // power, boot, bmc-reset, firmware, virtual-media
	Action string
	Args   map[string]string
}

func (d Danger) op(via string) safety.Op {
	args := map[string]string{"via": via}
	for k, v := range d.Args {
		args[k] = v
	}
	return safety.Op{Target: d.Target, Class: d.Class, Action: d.Action, Args: args}
}

func planFor(ctx context.Context, c *bmc.Conn, d Danger) (*bmc.Plan, bmc.Via, error) {
	return bmc.Call(ctx, c, func(b bmc.Backend) (*bmc.Plan, error) {
		switch d.Class {
		case "power":
			return b.PlanPower(ctx, d.Action)
		case "bmc-reset":
			return b.PlanBMCReset(ctx)
		case "boot":
			return b.PlanBootOverride(ctx, d.Args["boot_target"], d.Args["persistent"] == "true")
		case "firmware":
			return b.PlanFirmwareUpdate(ctx, d.Args["image_uri"])
		case "virtual-media":
			return b.PlanVirtualMedia(ctx, d.Args["image"], d.Action == "eject")
		}
		return nil, fmt.Errorf("unknown class %q", d.Class)
	})
}

// Planned is a plan ready for confirmation.
type Planned struct {
	Plan    *bmc.Plan `json:"plan"`
	Via     string    `json:"via"`
	Token   string    `json:"confirm_token,omitempty"`
	Expires time.Time `json:"expires,omitempty"`
	conn    *bmc.Conn
	proto   string
}

// Close releases the connection held by a plan.
func (p *Planned) Close() {
	if p != nil && p.conn != nil {
		p.conn.Close(context.Background())
	}
}

// Plan checks policy and builds a plan (nothing is sent to the BMC except reads).
func (a *App) Plan(ctx context.Context, actor string, d Danger, issueToken bool) (*Planned, error) {
	t, err := a.One(d.Target)
	if err != nil {
		return nil, err
	}
	d.Target = t.Name
	if err := a.Policy.Check(d.Class, t.Name, t.Allow, a.AdHoc == nil); err != nil {
		_ = a.Audit.Record(actor, "refused", d.op(""), map[string]any{"reason": err.Error()})
		return nil, err
	}
	c, err := bmc.Open(t)
	if err != nil {
		return nil, err
	}
	plan, via, err := planFor(ctx, c, d)
	if err != nil {
		c.Close(context.Background())
		return nil, err
	}
	p := &Planned{Plan: plan, Via: via.String(), conn: c, proto: via.Protocol}
	if issueToken && a.Tokens != nil {
		p.Token, p.Expires = a.Tokens.Issue(d.op(via.Protocol))
	}
	_ = a.Audit.Record(actor, "planned", d.op(via.Protocol), map[string]any{"summary": plan.Summary, "method": plan.Method, "path": plan.Path, "body": plan.Body})
	return p, nil
}

// Run executes an already confirmed plan (CLI path).
func (a *App) Run(ctx context.Context, actor string, d Danger, p *Planned) (string, error) {
	res, err := p.Plan.Execute(ctx)
	detail := map[string]any{"summary": p.Plan.Summary, "method": p.Plan.Method, "path": p.Plan.Path, "body": p.Plan.Body}
	proto := p.proto
	if err != nil {
		detail["error"] = err.Error()
		_ = a.Audit.Record(actor, "failed", d.op(proto), detail)
		return "", err
	}
	detail["result"] = res
	_ = a.Audit.Record(actor, "executed", d.op(proto), detail)
	return res, nil
}

// Confirm re-plans against current state, redeems the token, and executes
// (MCP path: the token proves a human saw this exact plan).
func (a *App) Confirm(ctx context.Context, actor string, d Danger, token string) (*Planned, string, error) {
	p, err := a.Plan(ctx, actor, d, false)
	if err != nil {
		return nil, "", err
	}
	defer p.Close()
	t, _ := a.One(d.Target)
	d.Target = t.Name
	proto := p.proto
	if err := a.Tokens.Redeem(d.op(proto), token); err != nil {
		_ = a.Audit.Record(actor, "token-rejected", d.op(proto), map[string]any{"reason": err.Error()})
		return p, "", err
	}
	res, err := a.Run(ctx, actor, d, p)
	return p, res, err
}

// TargetInfo describes a configured target for listing.
type TargetInfo struct {
	Name    string   `json:"name"`
	Redfish string   `json:"redfish,omitempty"`
	IPMI    string   `json:"ipmi,omitempty"`
	Vendor  string   `json:"vendor,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Allowed []string `json:"dangerous_allowed"`
}

// Targets lists targets with the dangerous classes effectively enabled.
func (a *App) Targets() []TargetInfo {
	var ts []*config.Target
	if a.AdHoc != nil {
		ts = []*config.Target{a.AdHoc}
	} else {
		for i := range a.Cfg.Targets {
			ts = append(ts, &a.Cfg.Targets[i])
		}
	}
	out := []TargetInfo{}
	for _, t := range ts {
		ti := TargetInfo{Name: t.Name, Redfish: t.Redfish, IPMI: t.IPMI, Vendor: t.Vendor, Tags: t.Tags, Allowed: []string{}}
		for _, c := range config.Classes {
			if a.Policy.Check(c, t.Name, t.Allow, a.AdHoc == nil) == nil {
				ti.Allowed = append(ti.Allowed, c)
			}
		}
		out = append(out, ti)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
