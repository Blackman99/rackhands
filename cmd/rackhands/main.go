// rackhands: one CLI and MCP server for out-of-band server management across
// Dell iDRAC, HPE iLO, Lenovo XCC, Supermicro and any Redfish BMC, with IPMI
// fallback. Read-only by default.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/Blackman99/rackhands/internal/app"
	"github.com/Blackman99/rackhands/internal/bmc"
	"github.com/Blackman99/rackhands/internal/config"
	"github.com/Blackman99/rackhands/internal/mcpserver"
	"github.com/Blackman99/rackhands/internal/redfish"
	"github.com/Blackman99/rackhands/internal/safety"
)

var version = "dev"

const usage = `rackhands — out-of-band server management for humans and agents
Redfish (Dell iDRAC, HPE iLO, Lenovo XCC, Supermicro, any DMTF BMC) with IPMI fallback.

Read (always available):
  rackhands targets                         configured BMCs and what is enabled
  rackhands inventory [TARGET...]           model, serial, BIOS/BMC firmware, CPU, memory
  rackhands health    [TARGET...]           rollup + every component that is not OK, storage included (exit 1 if any)
  rackhands storage   [TARGET...]           RAID controllers, drives (wear, predicted failure), volumes; --problems
  rackhands power     [TARGET...]           power state
  rackhands sensors   TARGET [--kind K] [--problems]
  rackhands events    TARGET [-n 30] [--min warning] [--all-logs]
  rackhands firmware  TARGET                installed firmware versions
  rackhands task      TARGET [ID] [--wait]  BMC tasks / iDRAC jobs; --wait follows one until it ends
  rackhands boot      TARGET                boot order and override
  rackhands vendors                         vendor quirks rackhands handles

Dangerous (off by default; need --allow CLASS, the target's allow: list, and confirmation):
  rackhands power TARGET on|off|force-off|restart|force-restart|cycle|nmi   class: power
  rackhands boot TARGET --override pxe|cd|hdd|bios|none [--persistent]      class: boot
  rackhands bmc-reset TARGET                                                class: bmc-reset
  rackhands firmware-update TARGET IMAGE_URL [--wait]                       class: firmware
  rackhands vmedia TARGET insert ISO_URL | eject                            class: virtual-media

TLS:
  rackhands tls-fingerprint TARGET          print leaf cert SHA-256 to paste into tls_fingerprint:
  rackhands pin TARGET                      alias for tls-fingerprint

Agents:
  rackhands mcp [--allow CLASS,...]         MCP server on stdio (dangerous tools only if allowed)

TARGET is a name from the config file, "all", or "tag:NAME". Default: all.
For tls-fingerprint, TARGET may also be omitted when --host is set.

Flags:
  --config FILE     targets file (default $RACKHANDS_CONFIG or ~/.config/rackhands/targets.yaml)
  --host URL        ad-hoc Redfish BMC instead of the config file (password in $RACKHANDS_PASSWORD)
  --ipmi HOST[:P]   ad-hoc IPMI address (alone, or as fallback for --host)
  --user NAME       ad-hoc username (default $RACKHANDS_USER)
  --vendor V        force vendor profile: dell, hpe, lenovo, supermicro, generic
  --json            machine-readable output
  --allow CLASSES   enable dangerous classes: power,boot,bmc-reset,firmware,virtual-media (or all)
  --yes TARGET      non-interactive confirmation; must repeat the exact target name
  --dry-run         show the plan for a dangerous command and stop
  --audit-log FILE  JSONL audit of dangerous requests (default ~/.local/state/rackhands/audit.jsonl; "off")
  --timeout D       per-BMC timeout (default 20s)
  --wait            firmware-update / task ID: follow the BMC task until it completes, fails or is staged
  --wait-timeout D  give up watching after D (default: the target's wait_timeout, 30m); implies --wait

Exit codes: 0 ok · 1 health problems found · 2 error · 3 refused or not confirmed
            4 task failed, was cancelled, vanished, or was still running at the deadline · 130 stopped watching (Ctrl-C)
`

type opts struct {
	config, host, ipmi, user, vendor, yes, audit, timeout, kind, min, override, waitTimeout string
	json, dryRun, problems, allLogs, persistent, help, version, wait                        bool
	allow                                                                                   []string
	n                                                                                       int
	args                                                                                    []string
}

func parse(argv []string) (*opts, error) {
	o := &opts{n: 30}
	strFlags := map[string]*string{"config": &o.config, "host": &o.host, "ipmi": &o.ipmi, "user": &o.user, "vendor": &o.vendor,
		"yes": &o.yes, "audit-log": &o.audit, "timeout": &o.timeout, "kind": &o.kind, "min": &o.min, "override": &o.override,
		"wait-timeout": &o.waitTimeout}
	boolFlags := map[string]*bool{"json": &o.json, "dry-run": &o.dryRun, "problems": &o.problems, "all-logs": &o.allLogs,
		"persistent": &o.persistent, "help": &o.help, "h": &o.help, "version": &o.version, "wait": &o.wait}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			o.args = append(o.args, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			o.args = append(o.args, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		next := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(argv) {
				return "", fmt.Errorf("flag --%s needs a value", name)
			}
			i++
			return argv[i], nil
		}
		switch {
		case name == "n":
			v, err := next()
			if err != nil {
				return nil, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("-n wants a positive number")
			}
			o.n = n
		case name == "allow":
			v, err := next()
			if err != nil {
				return nil, err
			}
			o.allow = append(o.allow, v)
		case strFlags[name] != nil:
			v, err := next()
			if err != nil {
				return nil, err
			}
			*strFlags[name] = v
		case boolFlags[name] != nil:
			b := true
			if hasVal {
				b = val == "true" || val == "1"
			}
			*boolFlags[name] = b
		default:
			return nil, fmt.Errorf("unknown flag %s (see rackhands help)", a)
		}
	}
	if o.waitTimeout != "" {
		o.wait = true
	}
	return o, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type exitErr struct {
	code int
	msg  string
}

func (e *exitErr) Error() string { return e.msg }

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parse(argv)
	if err != nil {
		fmt.Fprintln(stderr, "rackhands:", err)
		return 2
	}
	if o.version {
		fmt.Fprintln(stdout, "rackhands", version)
		return 0
	}
	if o.help || len(o.args) == 0 || o.args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := dispatch(ctx, o, stdin, stdout, stderr); err != nil {
		var ee *exitErr
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(stderr, "rackhands:", ee.msg)
			}
			return ee.code
		}
		fmt.Fprintln(stderr, "rackhands:", err)
		if errors.Is(err, safety.ErrNotAllowed) {
			return 3
		}
		return 2
	}
	return 0
}

func build(o *opts) (*app.App, error) {
	allowList := o.allow
	if env := os.Getenv("RACKHANDS_ALLOW"); env != "" && len(allowList) == 0 {
		allowList = []string{env}
	}
	allow := safety.ParseAllow(allowList)
	cl := []string{}
	for k := range allow {
		cl = append(cl, k)
	}
	if err := config.ValidateClasses(cl); err != nil {
		return nil, err
	}
	a := &app.App{Policy: safety.Policy{Runtime: allow}, Tokens: safety.NewTokens(2 * time.Minute)}
	if o.host != "" || o.ipmi != "" {
		user := o.user
		if user == "" {
			user = os.Getenv("RACKHANDS_USER")
		}
		name := o.host
		if name == "" {
			name = o.ipmi
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, "https://"), "http://")
		a.AdHoc = &config.Target{Name: name, Redfish: o.host, IPMI: o.ipmi, Username: user, PasswordEnv: "RACKHANDS_PASSWORD", Vendor: o.vendor, Timeout: o.timeout}
		if os.Getenv("RACKHANDS_PASSWORD") == "" {
			a.AdHoc.PasswordEnv = ""
		}
		a.Cfg = &config.File{}
	} else {
		cfg, err := config.Load(o.config)
		if err != nil {
			return nil, err
		}
		if o.timeout != "" || o.vendor != "" {
			for i := range cfg.Targets {
				if o.timeout != "" {
					cfg.Targets[i].Timeout = o.timeout
				}
				if o.vendor != "" {
					cfg.Targets[i].Vendor = o.vendor
				}
			}
		}
		a.Cfg = cfg
	}
	auditPath := o.audit
	if auditPath == "" && a.Cfg != nil && a.Cfg.AuditLog != "" {
		auditPath = a.Cfg.AuditLog
	}
	if auditPath == "" {
		auditPath = safety.DefaultAuditPath()
	}
	a.Audit = safety.NewAudit(auditPath)
	return a, nil
}

func dispatch(ctx context.Context, o *opts, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd, rest := o.args[0], o.args[1:]
	if cmd == "vendors" {
		return printVendors(stdout, o.json)
	}
	if cmd == "version" {
		fmt.Fprintln(stdout, "rackhands", version)
		return nil
	}
	if o.wait && cmd != "firmware-update" && cmd != "task" && cmd != "tasks" {
		return fmt.Errorf("--wait follows BMC tasks: use it with firmware-update or task, not %s", cmd)
	}
	if _, err := waitTimeout(o); err != nil {
		return err
	}
	a, err := build(o)
	if err != nil {
		return err
	}
	switch cmd {
	case "task", "tasks", "job", "jobs":
		return taskCmd(ctx, a, o, rest, stdout, stderr)
	case "tls-fingerprint", "pin":
		return tlsFingerprint(ctx, a, o, rest, stdout)
	case "targets":
		ts := a.Targets()
		if o.json {
			return emit(stdout, ts)
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "TARGET\tREDFISH\tIPMI\tTAGS\tDANGEROUS ENABLED")
		for _, t := range ts {
			en := strings.Join(t.Allowed, ",")
			if en == "" {
				en = "none (read-only)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, dash(t.Redfish), dash(t.IPMI), dash(strings.Join(t.Tags, ",")), en)
		}
		return tw.Flush()
	case "inventory":
		res, err := a.Fleet(ctx, rest, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
			return bmc.InventoryWithAccount(ctx, c)
		})
		if err != nil {
			return err
		}
		if o.json {
			if err := emit(stdout, res); err != nil {
				return err
			}
			return fleetErr(res)
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "TARGET\tVIA\tVENDOR\tMODEL\tSERIAL\tBIOS\tBMC\tCPU\tMEM\tPOWER\tACCOUNT")
		var warns []string
		for _, r := range res {
			if r.Error != "" {
				fmt.Fprintf(tw, "%s\t%s\tERROR: %s\n", r.Target, dash(r.Via), r.Error)
				continue
			}
			inv := r.Data.(*bmc.Inventory)
			cpu := "-"
			if inv.CPUCount > 0 {
				cpu = fmt.Sprintf("%d×", inv.CPUCount)
			}
			mem := "-"
			if inv.MemoryGiB > 0 {
				mem = fmt.Sprintf("%g GiB", inv.MemoryGiB)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Target, r.Via, inv.Vendor, dash(inv.Model), dash(inv.Serial), dash(inv.BIOS),
				dash(bmcLabel(inv)), cpu, mem, dash(inv.PowerState), accountLabel(inv.Account))
			if inv.Account != nil && inv.Account.Warning != "" {
				warns = append(warns, fmt.Sprintf("WARN  %s: %s", r.Target, inv.Account.Warning))
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, w := range warns {
			fmt.Fprintln(stderr, w)
		}
		return fleetErr(res)
	case "health":
		res, err := a.Fleet(ctx, rest, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
			return bmc.Call(ctx, c, func(b bmc.Backend) (*bmc.HealthReport, error) { return b.Health(ctx) })
		})
		if err != nil {
			return err
		}
		bad, failed := false, false
		for _, r := range res {
			if r.Error != "" {
				failed = true
			} else if h := r.Data.(*bmc.HealthReport); h.Overall != bmc.OK {
				bad = true
			}
		}
		if o.json {
			if err := emit(stdout, res); err != nil {
				return err
			}
		} else {
			for _, r := range res {
				if r.Error != "" {
					fmt.Fprintf(stdout, "%-9s %s  (via %s) %s\n", "ERROR", r.Target, dash(r.Via), r.Error)
					continue
				}
				h := r.Data.(*bmc.HealthReport)
				fmt.Fprintf(stdout, "%-9s %s  (%s, via %s)\n", strings.ToUpper(h.Overall), r.Target, h.Vendor, r.Via)
				for _, p := range h.Problems {
					d := ""
					if p.Detail != "" {
						d = "  — " + p.Detail
					}
					fmt.Fprintf(stdout, "   %-9s %s%s\n", p.Health, p.Component, d)
				}
				fmt.Fprintf(stdout, "   %-9s %s\n", "storage", storageLine(h))
			}
		}
		if failed {
			return &exitErr{code: 2}
		}
		if bad {
			return &exitErr{code: 1}
		}
		return nil
	case "storage", "disks", "raid":
		return storageCmd(ctx, a, o, rest, stdout)
	case "power":
		if len(rest) >= 2 {
			return dangerous(ctx, a, o, stdin, stdout, stderr, app.Danger{Target: rest[0], Class: "power", Action: rest[1]})
		}
		res, err := a.Fleet(ctx, rest, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
			return bmc.Call(ctx, c, func(b bmc.Backend) (string, error) { return b.PowerState(ctx) })
		})
		if err != nil {
			return err
		}
		if o.json {
			if err := emit(stdout, res); err != nil {
				return err
			}
			return fleetErr(res)
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		for _, r := range res {
			if r.Error != "" {
				fmt.Fprintf(tw, "%s\tERROR\t%s\n", r.Target, r.Error)
				continue
			}
			fmt.Fprintf(tw, "%s\t%v\tvia %s\n", r.Target, r.Data, r.Via)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		return fleetErr(res)
	case "sensors":
		if len(rest) != 1 {
			return errors.New("usage: rackhands sensors TARGET")
		}
		r, err := oneRead(ctx, a, rest[0], func(ctx context.Context, b bmc.Backend) (any, error) { return b.Sensors(ctx) })
		if err != nil {
			return err
		}
		var out []bmc.Sensor
		for _, s := range r.Data.([]bmc.Sensor) {
			if o.kind != "" && !strings.EqualFold(s.Kind, o.kind) {
				continue
			}
			if o.problems && (s.Health == "" || s.Health == bmc.OK) {
				continue
			}
			out = append(out, s)
		}
		if o.json {
			r.Data = out
			return emit(stdout, r)
		}
		fmt.Fprintf(stdout, "%s  via %s\n", r.Target, r.Via)
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "KIND\tNAME\tREADING\tCRITICAL AT\tHEALTH")
		for _, s := range out {
			rd := dash(s.State)
			if s.Reading != nil {
				rd = fmt.Sprintf("%g %s", *s.Reading, s.Unit)
			}
			crit := "-"
			switch {
			case s.UpperCrit != nil && s.LowerCrit != nil:
				crit = fmt.Sprintf("<%g or >%g", *s.LowerCrit, *s.UpperCrit)
			case s.UpperCrit != nil:
				crit = fmt.Sprintf(">%g", *s.UpperCrit)
			case s.LowerCrit != nil:
				crit = fmt.Sprintf("<%g", *s.LowerCrit)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Kind, s.Name, rd, crit, dash(s.Health))
		}
		return tw.Flush()
	case "events", "sel":
		if len(rest) != 1 {
			return errors.New("usage: rackhands events TARGET")
		}
		r, err := oneRead(ctx, a, rest[0], func(ctx context.Context, b bmc.Backend) (any, error) { return b.Events(ctx, 2000, o.allLogs) })
		if err != nil {
			return err
		}
		min := bmc.Rank(o.min)
		out := []bmc.Event{}
		for _, e := range r.Data.([]bmc.Event) {
			if bmc.Rank(e.Severity) >= min {
				out = append(out, e)
			}
			if len(out) >= o.n {
				break
			}
		}
		if o.json {
			r.Data = out
			return emit(stdout, r)
		}
		fmt.Fprintf(stdout, "%s  via %s\n", r.Target, r.Via)
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		for _, e := range out {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", dash(e.Time), e.Severity, e.Source, e.Message)
		}
		return tw.Flush()
	case "firmware":
		if len(rest) != 1 {
			return errors.New("usage: rackhands firmware TARGET")
		}
		r, err := oneRead(ctx, a, rest[0], func(ctx context.Context, b bmc.Backend) (any, error) { return b.Firmware(ctx) })
		if err != nil {
			return err
		}
		if o.json {
			return emit(stdout, r)
		}
		fmt.Fprintf(stdout, "%s  via %s\n", r.Target, r.Via)
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		for _, f := range r.Data.([]bmc.Firmware) {
			fmt.Fprintf(tw, "%s\t%s\n", f.Name, f.Version)
		}
		return tw.Flush()
	case "boot":
		if len(rest) != 1 {
			return errors.New("usage: rackhands boot TARGET [--override X]")
		}
		if o.override != "" {
			return dangerous(ctx, a, o, stdin, stdout, stderr, app.Danger{Target: rest[0], Class: "boot", Action: "override",
				Args: map[string]string{"boot_target": o.override, "persistent": strconv.FormatBool(o.persistent)}})
		}
		r, err := oneRead(ctx, a, rest[0], func(ctx context.Context, b bmc.Backend) (any, error) { return b.Boot(ctx) })
		if err != nil {
			return err
		}
		if o.json {
			return emit(stdout, r)
		}
		b := r.Data.(*bmc.Boot)
		fmt.Fprintf(stdout, "%s  via %s\n", r.Target, r.Via)
		fmt.Fprintf(stdout, "override  %s (%s), mode %s\n", dash(b.OverrideTarget), dash(b.OverrideMode), dash(b.BootMode))
		for i, e := range b.Order {
			fmt.Fprintf(stdout, "%2d. %s\n", i+1, e)
		}
		return nil
	case "bmc-reset":
		if len(rest) != 1 {
			return errors.New("usage: rackhands bmc-reset TARGET")
		}
		return dangerous(ctx, a, o, stdin, stdout, stderr, app.Danger{Target: rest[0], Class: "bmc-reset", Action: "reset"})
	case "firmware-update":
		if len(rest) != 2 {
			return errors.New("usage: rackhands firmware-update TARGET IMAGE_URL")
		}
		return dangerous(ctx, a, o, stdin, stdout, stderr, app.Danger{Target: rest[0], Class: "firmware", Action: "update", Args: map[string]string{"image_uri": rest[1]}})
	case "vmedia":
		switch {
		case len(rest) == 3 && rest[1] == "insert":
			return dangerous(ctx, a, o, stdin, stdout, stderr, app.Danger{Target: rest[0], Class: "virtual-media", Action: "insert", Args: map[string]string{"image": rest[2]}})
		case len(rest) == 2 && rest[1] == "eject":
			return dangerous(ctx, a, o, stdin, stdout, stderr, app.Danger{Target: rest[0], Class: "virtual-media", Action: "eject", Args: map[string]string{"image": ""}})
		}
		return errors.New("usage: rackhands vmedia TARGET insert ISO_URL | eject")
	case "mcp":
		srv := mcpserver.New(a, version)
		enabled := []string{}
		for _, c := range config.Classes {
			if a.Policy.Enabled(c) {
				enabled = append(enabled, c)
			}
		}
		if len(enabled) == 0 {
			fmt.Fprintln(stderr, "rackhands mcp: read-only (no dangerous tools registered)")
		} else {
			fmt.Fprintf(stderr, "rackhands mcp: dangerous classes enabled: %s (two-step confirmation, audit %s)\n", strings.Join(enabled, ","), dash(a.Audit.Path()))
		}
		return srv.Run(ctx, &mcp.StdioTransport{})
	}
	return fmt.Errorf("unknown command %q (see rackhands help)", cmd)
}

// accountLabel: "admin (Administrator, can power off)", "agent (ReadOnly)".
func accountLabel(a *bmc.Account) string {
	if a == nil {
		return "-"
	}
	role := a.Role
	if a.CanControl != nil && *a.CanControl {
		role += ", can power off"
	}
	if a.User == "" {
		return role
	}
	return a.User + " (" + role + ")"
}

func bmcLabel(inv *bmc.Inventory) string {
	if strings.HasPrefix(inv.BMCFirmware, inv.BMC) {
		return inv.BMCFirmware
	}
	return strings.TrimSpace(inv.BMC + " " + inv.BMCFirmware)
}

func fleetErr(res []app.Result) error {
	for _, r := range res {
		if r.Error != "" {
			return &exitErr{code: 2}
		}
	}
	return nil
}

func oneRead(ctx context.Context, a *app.App, target string, fn func(context.Context, bmc.Backend) (any, error)) (app.Result, error) {
	if target == "all" || strings.HasPrefix(target, "tag:") {
		return app.Result{}, fmt.Errorf("%q selects several machines; this command reads one, so name it (see `rackhands targets`)", target)
	}
	res, err := a.Fleet(ctx, []string{target}, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
		return bmc.Call(ctx, c, func(b bmc.Backend) (any, error) { return fn(ctx, b) })
	})
	if err != nil {
		return app.Result{}, err
	}
	if res[0].Error != "" {
		return res[0], fmt.Errorf("%s (via %s): %s", res[0].Target, dash(res[0].Via), res[0].Error)
	}
	return res[0], nil
}

func dangerous(ctx context.Context, a *app.App, o *opts, stdin io.Reader, stdout, stderr io.Writer, d app.Danger) error {
	p, err := a.Plan(ctx, "cli", d, false)
	if err != nil {
		return err
	}
	defer p.Close()
	name := d.Target
	if t, err := a.One(d.Target); err == nil {
		name = t.Name
	}
	fmt.Fprintf(stderr, "PLAN  %s\n      %s %s", p.Plan.Summary, p.Plan.Method, p.Plan.Path)
	if len(p.Plan.Body) > 0 {
		b, _ := json.Marshal(p.Plan.Body)
		fmt.Fprintf(stderr, " %s", b)
	}
	fmt.Fprintf(stderr, "\n      via %s\n", p.Via)
	follow := o.wait && d.Class == "firmware"
	var deadline time.Duration
	if follow {
		deadline, _ = waitTimeout(o)
		if t, err := a.One(d.Target); deadline == 0 && err == nil {
			deadline = t.WaitTimeoutDur()
		}
		fmt.Fprintf(stderr, "      then watch the task it starts (reads only) until it ends, at most %s\n", fmtDur(deadline))
	}
	if o.dryRun {
		if o.json {
			return emit(stdout, map[string]any{"status": "dry-run", "plan": p.Plan, "via": p.Via})
		}
		fmt.Fprintln(stderr, "dry run: nothing sent")
		return nil
	}
	switch {
	case o.yes != "":
		if o.yes != name {
			return &exitErr{code: 3, msg: fmt.Sprintf("--yes must repeat the target name %q; refusing", name)}
		}
	case isTerminal(stdin):
		fmt.Fprintf(stderr, "Type the target name (%s) to confirm: ", name)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(line) != name {
			return &exitErr{code: 3, msg: "not confirmed; nothing sent"}
		}
	default:
		return &exitErr{code: 3, msg: "not confirmed: no terminal; pass --yes " + name + " to confirm non-interactively"}
	}
	res, err := a.Run(ctx, "cli", d, p)
	if err != nil {
		return err
	}
	ref, _ := p.Plan.Task()
	if !follow {
		if o.json {
			out := map[string]any{"status": "done", "summary": p.Plan.Summary, "via": p.Via, "result": res}
			if ref != "" {
				out["task_ref"] = ref
			}
			return emit(stdout, out)
		}
		fmt.Fprintf(stdout, "DONE  %s: %s\n", name, res)
		if ref != "" {
			fmt.Fprintf(stderr, "      follow it: %s task %s %s --wait\n", self(o), name, bmc.ShortRef(ref))
		}
		return nil
	}
	w, pr, err := watchOpts(o, stderr, name, ref)
	if err != nil {
		return err
	}
	w.Timeout = deadline
	if !o.json {
		fmt.Fprintf(stderr, "SENT  %s: %s\n", name, res)
		if ref != "" {
			fmt.Fprintf(stderr, "      watching %s until it ends (at most %s; Ctrl-C stops watching, not the update)\n", bmc.ShortRef(ref), fmtDur(deadline))
		}
	}
	out, err := a.Follow(ctx, "cli", d, p, w)
	if pr != nil {
		pr.end()
	}
	if err != nil {
		return fmt.Errorf("the update was sent (%s) but watching it failed: %w", res, err)
	}
	if o.json {
		if err := emit(stdout, map[string]any{"status": "done", "summary": p.Plan.Summary, "via": p.Via, "result": res, "task": out}); err != nil {
			return err
		}
		return codeErr(outcomeCode(out))
	}
	return codeErr(verdict(o, stdout, stderr, out))
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func emit(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func tlsFingerprint(ctx context.Context, a *app.App, o *opts, rest []string, stdout io.Writer) error {
	var addr string
	var timeout time.Duration
	switch {
	case len(rest) == 1 && rest[0] != "all" && !strings.HasPrefix(rest[0], "tag:"):
		t, err := a.One(rest[0])
		if err != nil {
			return err
		}
		if t.Redfish == "" {
			return fmt.Errorf("target %q has no redfish: address", t.Name)
		}
		addr = t.Redfish
		timeout = t.TimeoutDur()
	case o.host != "":
		addr = o.host
		if o.timeout != "" {
			if d, err := time.ParseDuration(o.timeout); err == nil {
				timeout = d
			}
		}
	case len(rest) == 0:
		return errors.New("usage: rackhands tls-fingerprint TARGET  (or --host URL)")
	default:
		return errors.New("tls-fingerprint takes one named target (or --host)")
	}
	fp, err := redfish.FetchLeafFingerprint(ctx, addr, timeout)
	if err != nil {
		return err
	}
	if o.json {
		return emit(stdout, map[string]string{
			"address":               addr,
			"tls_fingerprint":       fp,
			"tls_fingerprint_colon": redfish.FormatFingerprint(fp),
		})
	}
	fmt.Fprintf(stdout, "# paste into targets.yaml for this BMC:\n#   tls_fingerprint: %s\n%s\n", fp, fp)
	return nil
}

func printVendors(w io.Writer, js bool) error {
	type row struct {
		Vendor, BMC, EventLogs, Quirks string
	}
	q := map[string]string{
		"Dell":       "hides Previous-/Available- firmware copies; DellSystem *RollupStatus subsystem health; SEL under iDRAC.Embedded.1; firmware jobs from the iDRAC job queue (Oem.Dell JobState wins over TaskState, Scheduled = staged for the next reboot); PERC storage under RAID.SL.*/RAID.Integrated.* with DellPhysicalDisk/DellVolume RaidStatus, empty AHCI/CPU subsystems skipped",
		"HPE":        "iLO 4 (Oem.Hp) and iLO 5/6 (Oem.Hpe) AggregateHealthStatus; Manager.Reset sent without ResetType; IML severity Caution→Warning; OEM virtual media actions; iLO 5 SimpleUpdate has no Task, so the flash is followed on UpdateService Oem.Hpe.State; Smart Array read from the OEM SmartStorage tree (ArrayControllers, DiskDrives, LogicalDrives) when DMTF Storage is missing, as on iLO 4",
		"Lenovo":     "If-Match ETag on every PATCH (XCC rejects PATCH without it); PlatformLog as the hardware log; firmware tasks via a DSP0266 task monitor; RAID controllers from the Storage Controllers collection",
		"Supermicro": "Log1 as SEL; X11 OEM-only virtual media reported as unsupported instead of failing mid-way; drives linked from /Chassis, volumes with only VolumeType mapped to a RAID level, rebuild progress from Operations",
		"Generic":    "pure DMTF schema: Thermal/Power or Sensors, LogServices with LogEntryType=SEL, SimpleUpdate, InsertMedia, TaskService tasks and task monitors; Storage, or SimpleStorage when that is all there is",
	}
	var rows []row
	for _, v := range bmc.Profiles() {
		p := bmc.ProfileFor(v)
		rows = append(rows, row{Vendor: v, BMC: p.BMC, EventLogs: strings.Join(p.Logs, ", "), Quirks: q[v]})
	}
	if js {
		return emit(w, rows)
	}
	for _, r := range rows {
		fmt.Fprintf(w, "%-11s %-6s logs: %s\n            %s\n", r.Vendor, r.BMC, r.EventLogs, r.Quirks)
	}
	fmt.Fprintln(w, "\nAny vendor: falls back to IPMI 2.0 (power, sensors, SEL, FRU, BMC reset, boot device) when Redfish is down or missing.")
	fmt.Fprintln(w, "Storage (controllers, drives, volumes) is Redfish-only: IPMI has no RAID or drive data, so rackhands says so instead of guessing.")
	return nil
}

// storageLine is the one-line storage verdict under each health report.
func storageLine(h *bmc.HealthReport) string {
	if h.Storage != nil {
		s := h.Storage.Summary()
		if h.Storage.Source != "" && h.Storage.Source != "Storage" {
			s += "  (" + h.Storage.Source + ")"
		}
		return s
	}
	for _, n := range h.Notes {
		if strings.HasPrefix(n, "storage not") {
			return strings.TrimPrefix(strings.TrimPrefix(n, "storage not read: "), "storage ")
		}
	}
	return "not read"
}

func storageCmd(ctx context.Context, a *app.App, o *opts, rest []string, stdout io.Writer) error {
	res, err := a.Fleet(ctx, rest, func(ctx context.Context, c *bmc.Conn) (any, bmc.Via, error) {
		return bmc.Call(ctx, c, func(b bmc.Backend) (*bmc.StorageReport, error) { return b.Storage(ctx) })
	})
	if err != nil {
		return err
	}
	bad, failed := false, false
	for _, r := range res {
		if r.Error != "" {
			failed = true
		} else if s := r.Data.(*bmc.StorageReport); s.Health != "" && s.Health != bmc.OK {
			bad = true
		}
	}
	if o.json {
		if o.problems {
			for _, r := range res {
				if s, ok := r.Data.(*bmc.StorageReport); ok {
					cp := *s
					cp.Controllers, cp.Drives, cp.Volumes = nil, nil, nil
					for _, c := range s.Controllers {
						if notOK(c.Health) {
							cp.Controllers = append(cp.Controllers, c)
						}
					}
					for _, d := range s.Drives {
						if notOK(d.Health) {
							cp.Drives = append(cp.Drives, d)
						}
					}
					for _, v := range s.Volumes {
						if notOK(v.Health) {
							cp.Volumes = append(cp.Volumes, v)
						}
					}
					r.Data = &cp
				}
			}
		}
		if err := emit(stdout, res); err != nil {
			return err
		}
	} else {
		for i, r := range res {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			if r.Error != "" {
				fmt.Fprintf(stdout, "%-9s %s  (via %s) %s\n", "ERROR", r.Target, dash(r.Via), r.Error)
				continue
			}
			printStorage(stdout, r, r.Data.(*bmc.StorageReport), o.problems)
		}
	}
	if failed {
		return &exitErr{code: 2}
	}
	if bad {
		return &exitErr{code: 1}
	}
	return nil
}

func notOK(h string) bool { return h != "" && h != bmc.OK }

func printStorage(w io.Writer, r app.Result, s *bmc.StorageReport, problemsOnly bool) {
	verdict := strings.ToUpper(s.Health)
	if verdict == "" {
		verdict = "-"
	}
	src := ""
	if s.Source != "" {
		src = ", " + s.Source
	}
	fmt.Fprintf(w, "%-9s %s  (%s, via %s%s)  %s\n", verdict, r.Target, s.Vendor, r.Via, src, s.Summary())
	keep := func(h string) bool { return !problemsOnly || notOK(h) }
	var rows int

	var ctl [][]string
	for _, c := range s.Controllers {
		if keep(c.Health) {
			model := c.Model
			if strings.Contains(c.Name, model) {
				model = ""
			}
			ctl = append(ctl, []string{c.Name, model, c.Firmware, c.Health})
		}
	}
	rows += table(w, []string{"CONTROLLER", "MODEL", "FIRMWARE", "HEALTH"}, ctl)

	var drv [][]string
	for _, d := range s.Drives {
		if keep(d.Health) {
			life := ""
			if d.LifeLeftPercent != nil {
				life = fmt.Sprintf("%g%%", *d.LifeLeftPercent)
			}
			loc := d.Location
			if strings.EqualFold(loc, d.Name) {
				loc = ""
			}
			drv = append(drv, []string{d.Name, strings.TrimSpace(d.Protocol + " " + d.MediaType), bmc.HumanBytes(d.CapacityBytes),
				d.Health, life, loc, driveNote(d)})
		}
	}
	rows += table(w, []string{"DRIVE", "MEDIA", "SIZE", "HEALTH", "LIFE LEFT", "LOCATION", "NOTE"}, drv)

	var vol [][]string
	for _, v := range s.Volumes {
		if keep(v.Health) {
			n := ""
			if v.Drives > 0 {
				n = strconv.Itoa(v.Drives)
			}
			state := v.RAIDStatus
			if state == "" {
				state = v.State
			}
			if v.Operation != "" {
				state = strings.TrimPrefix(state+", "+v.Operation, ", ")
			}
			vol = append(vol, []string{v.Name, v.RAIDType, bmc.HumanBytes(v.CapacityBytes), v.Health, n, state})
		}
	}
	rows += table(w, []string{"VOLUME", "RAID", "SIZE", "HEALTH", "DRIVES", "STATE"}, vol)

	if problemsOnly && rows == 0 && s.Source != "" {
		fmt.Fprintln(w, "  nothing to report: every controller, drive and volume is OK")
	}
	for _, n := range s.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
}

// table prints an indented section, leaving out columns (other than the
// first) that are empty in every row, and returns the number of rows.
func table(w io.Writer, head []string, rows [][]string) int {
	if len(rows) == 0 {
		return 0
	}
	used := make([]bool, len(head))
	used[0] = true
	for _, r := range rows {
		for i, c := range r {
			used[i] = used[i] || c != ""
		}
	}
	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	line := func(cells []string, blank func(string) string) {
		var out []string
		for i, c := range cells {
			if used[i] {
				if i < len(cells)-1 {
					c = blank(c)
				}
				out = append(out, c)
			}
		}
		fmt.Fprintln(tw, "  "+strings.Join(out, "\t"))
	}
	line(head, func(c string) string { return c })
	for _, r := range rows {
		line(r, dash)
	}
	_ = tw.Flush()
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		fmt.Fprintln(w, strings.TrimRight(l, " "))
	}
	return len(rows)
}

// driveNote explains a drive's verdict in a few words, or stays empty.
func driveNote(d bmc.Drive) string {
	var n []string
	if d.RAIDStatus != "" && d.RAIDStatus != "Online" && d.RAIDStatus != "OK" {
		n = append(n, d.RAIDStatus)
	}
	if d.FailurePredicted != nil && *d.FailurePredicted {
		n = append(n, "failure predicted")
	}
	if d.LifeLeftPercent != nil && *d.LifeLeftPercent < bmc.LowLifeLeftPercent {
		n = append(n, "media nearly worn out")
	}
	if d.BMCHealth != "" && d.BMCHealth != d.Health {
		n = append(n, "BMC says "+d.BMCHealth)
	}
	if d.Operation != "" {
		n = append(n, d.Operation)
	}
	if d.State != "" && d.State != "Enabled" {
		n = append(n, d.State)
	}
	return strings.Join(n, ", ")
}
