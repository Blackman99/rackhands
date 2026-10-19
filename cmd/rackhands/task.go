package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/Blackman99/rackhands/internal/app"
	"github.com/Blackman99/rackhands/internal/bmc"
	"github.com/Blackman99/rackhands/internal/redfish"
)

// Poll cadence for --wait (tests shorten it).
var (
	pollEvery = 2 * time.Second
	pollMax   = 15 * time.Second
)

// Exit codes beyond 0-3 (see usage).
const (
	exitTaskNotDone = 4   // failed, cancelled, needs attention, vanished, or still running at the deadline
	exitInterrupted = 130 // Ctrl-C while watching (the task itself keeps running)
)

// waitTimeout is --wait-timeout (0: the target's wait_timeout).
func waitTimeout(o *opts) (time.Duration, error) {
	if o.waitTimeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(o.waitTimeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--wait-timeout wants a duration like 45m or 90s, got %q", o.waitTimeout)
	}
	return d, nil
}

// self is how to call rackhands again with the same config, for hints.
func self(o *opts) string {
	s := "rackhands"
	switch {
	case o.host != "":
		s += " --host " + o.host
		if o.user != "" {
			s += " --user " + o.user
		}
	case o.ipmi != "":
		s += " --ipmi " + o.ipmi
	case o.config != "":
		s += " --config " + o.config
	}
	return s
}

func kindLabel(k string) string {
	switch k {
	case bmc.KindIDRACJob:
		return "iDRAC job"
	case bmc.KindILOFlash:
		return "iLO flash"
	case bmc.KindJob:
		return "Redfish job"
	}
	return "Redfish task"
}

// fmtDur: 300ms, 42s, 3m02s, 30m, 1h04m.
func fmtDur(d time.Duration) string {
	if d > 0 && d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func pctLabel(t *bmc.Task) string {
	if t == nil || t.Percent == nil {
		return "-"
	}
	return fmt.Sprintf("%d%%", *t.Percent)
}

func bar(p *int) string {
	if p == nil {
		return ""
	}
	n := (*p + 5) / 10
	return strings.Repeat("█", n) + strings.Repeat("░", 10-n)
}

// taskLabel names a task in a sentence: "JID_001" (iDRAC ids say what they
// are), "iLO flash", "task 7", or the monitor URI.
func taskLabel(t *bmc.Task, ref string) string {
	id := bmc.ShortRef(ref)
	if t != nil && t.ID != "" {
		id = t.ID
	}
	switch {
	case t != nil && t.Kind == bmc.KindILOFlash:
		return "iLO flash"
	case t != nil && t.Kind == bmc.KindIDRACJob, strings.HasPrefix(id, "JID_"), strings.HasPrefix(id, "/"), strings.Contains(id, "://"):
		return id
	}
	return "task " + id
}

// progress renders TaskUpdates on stderr: one line rewritten in place on a
// terminal, a new line per change otherwise (CI logs, pipes).
type progress struct {
	w      io.Writer
	tty    bool
	width  int
	target string
	ref    string
	last   string
	open   bool
}

func newProgress(w io.Writer, target, ref string) *progress {
	p := &progress{w: w, target: target, ref: ref, width: 100}
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		p.tty = true
		if wd, _, err := term.GetSize(int(f.Fd())); err == nil && wd > 20 {
			p.width = wd
		}
	}
	return p
}

func (p *progress) update(u bmc.TaskUpdate) {
	t := u.Task
	var body string
	switch {
	case u.Err != nil:
		why := "not answering"
		var he *redfish.HTTPError
		if errors.As(u.Err, &he) {
			why = fmt.Sprintf("busy (HTTP %d)", he.Status)
		}
		body = fmt.Sprintf("BMC %s, retrying; it may be restarting to apply its own firmware", why)
	case u.Waiting:
		body = fmt.Sprintf("%s  waiting for the BMC to start (it still shows the previous run)", t.State)
	default:
		body = fmt.Sprintf("%-11s", t.State)
		if t.Percent != nil {
			body += fmt.Sprintf(" %3d%% %s", *t.Percent, bar(t.Percent))
		}
		if t.Message != "" {
			body += "  " + t.Message
		}
	}
	head := fmt.Sprintf("WAIT  %s %s  ", p.target, taskLabel(t, p.ref))
	clock := "  " + fmtDur(u.Elapsed.Truncate(time.Second))
	if !p.tty {
		if head+body == p.last {
			return
		}
		p.last = head + body
		fmt.Fprintln(p.w, head+body+clock)
		return
	}
	line := head + body
	if room := p.width - 1 - len([]rune(clock)); len([]rune(line)) > room && room > 10 {
		line = string([]rune(line)[:room-1]) + "…"
	}
	fmt.Fprint(p.w, "\r\x1b[K"+line+clock)
	p.open = true
}

func (p *progress) end() {
	if p.open {
		fmt.Fprint(p.w, "\r\x1b[K")
		p.open = false
	}
}

// verdict prints how a watch ended (stdout) and returns the exit code.
func verdict(o *opts, stdout, stderr io.Writer, out *bmc.Outcome) int {
	t := out.Task
	id := taskLabel(t, out.Ref)
	resume := fmt.Sprintf("%s task %s %s --wait", self(o), out.Target, bmc.ShortRef(out.Ref))
	took := ""
	if t != nil && t.Duration() > 0 {
		took = " in " + fmtDur(t.Duration())
	} else if out.Polls > 1 {
		took = " in " + fmtDur(time.Duration(out.Elapsed*float64(time.Second)))
	}
	msg := ""
	if t != nil && t.Message != "" {
		msg = " — " + t.Message
	}
	state := func(word string) string {
		if t != nil && !strings.EqualFold(t.State, word) {
			return " (" + t.State + ")"
		}
		return ""
	}
	where := func() string {
		if t == nil {
			return "never answered"
		}
		if t.Percent != nil {
			return fmt.Sprintf("%s at %d%%", t.State, *t.Percent)
		}
		return t.State
	}
	watched := fmtDur(time.Duration(out.Elapsed * float64(time.Second)))
	code := 0
	switch out.Verdict {
	case bmc.PhaseCompleted:
		p := ""
		if t != nil && t.Percent != nil && *t.Percent != 100 {
			p = fmt.Sprintf(" at %d%%", *t.Percent)
		}
		fmt.Fprintf(stdout, "DONE  %s: %s completed%s%s%s\n", out.Target, id, state("completed"), took+p, msg)
		if t != nil && t.Health == bmc.Warning {
			fmt.Fprintf(stderr, "WARN  %s: the BMC finished %s with warnings; read them with: %s task %s %s\n", out.Target, id, self(o), out.Target, bmc.ShortRef(out.Ref))
		}
	case bmc.PhaseScheduled:
		fmt.Fprintf(stdout, "STAGED  %s: %s is %s%s\n", out.Target, id, t.State, msg)
		fmt.Fprintf(stdout, "        The BMC applies it at the next host reboot. Reboot when it suits you: %s --allow power power %s restart\n", self(o), out.Target)
	case bmc.PhaseFailed:
		fmt.Fprintf(stdout, "FAILED  %s: %s failed%s%s%s\n", out.Target, id, state("failed"), took, msg)
		code = exitTaskNotDone
	case bmc.PhaseCancelled:
		fmt.Fprintf(stdout, "CANCELLED  %s: %s was cancelled%s%s\n", out.Target, id, took, msg)
		code = exitTaskNotDone
	case bmc.PhaseAttention:
		fmt.Fprintf(stdout, "BLOCKED  %s: %s needs someone at the BMC (%s)%s\n", out.Target, id, t.State, msg)
		code = exitTaskNotDone
	case bmc.VerdictTimeout:
		fmt.Fprintf(stdout, "TIMEOUT  %s: %s is still %s at the %s deadline. It keeps running on the BMC; keep watching: %s\n", out.Target, id, where(), fmtDur(time.Duration(out.Deadline*float64(time.Second))), resume)
		code = exitTaskNotDone
	case bmc.VerdictInterrupted:
		fmt.Fprintf(stdout, "STOPPED  %s: stopped watching %s (%s) after %s. The task keeps running on the BMC; resume: %s\n", out.Target, id, where(), watched, resume)
		code = exitInterrupted
	case bmc.VerdictVanished:
		fmt.Fprintf(stdout, "GONE  %s: %s disappeared from the BMC after %s (last seen %s); the BMC may have restarted. Check the result: %s firmware %s\n", out.Target, id, watched, where(), self(o), out.Target)
		code = exitTaskNotDone
	case bmc.VerdictNoTask:
		fmt.Fprintf(stdout, "NO TASK  %s: the BMC returned no task or job to follow, so there is nothing to watch.\n", out.Target)
		fmt.Fprintf(stdout, "      look for one:       %s task %s\n", self(o), out.Target)
		fmt.Fprintf(stdout, "      check the version:  %s firmware %s\n", self(o), out.Target)
	default:
		fmt.Fprintf(stdout, "%s  %s: %s\n", strings.ToUpper(out.Verdict), out.Target, id)
		code = exitTaskNotDone
	}
	if out.Note != "" && out.Verdict != bmc.VerdictVanished && out.Verdict != bmc.VerdictNoTask {
		fmt.Fprintf(stderr, "      note: %s\n", out.Note)
	}
	return code
}

func watchOpts(o *opts, stderr io.Writer, target, ref string) (bmc.WatchOptions, *progress, error) {
	d, err := waitTimeout(o)
	if err != nil {
		return bmc.WatchOptions{}, nil, err
	}
	w := bmc.WatchOptions{Timeout: d, Every: pollEvery, MaxEvery: pollMax}
	var pr *progress
	if !o.json {
		pr = newProgress(stderr, target, ref)
		w.OnUpdate = pr.update
	}
	return w, pr, nil
}

// taskCmd: rackhands task TARGET [ID|URI] [--wait [--wait-timeout D]]
func taskCmd(ctx context.Context, a *app.App, o *opts, rest []string, stdout, stderr io.Writer) error {
	if len(rest) == 0 || len(rest) > 2 {
		return errors.New("usage: rackhands task TARGET [TASK_ID|URI] [--wait] [--wait-timeout 30m]")
	}
	target := rest[0]
	if len(rest) == 1 {
		if o.wait {
			return fmt.Errorf("--wait needs a task id; list them with: %s task %s", self(o), target)
		}
		r, err := oneRead(ctx, a, target, func(ctx context.Context, b bmc.Backend) (any, error) { return b.Tasks(ctx) })
		if err != nil {
			return err
		}
		if o.json {
			return emit(stdout, r)
		}
		ts := r.Data.([]bmc.Task)
		fmt.Fprintf(stdout, "%s  via %s\n", r.Target, r.Via)
		if len(ts) == 0 {
			fmt.Fprintln(stdout, "no tasks or jobs on this BMC")
			return nil
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tKIND\tSTATE\t%\tNAME\tMESSAGE")
		for i := range ts {
			t := &ts[i]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, kindLabel(t.Kind), t.State, pctLabel(t), dash(t.Name), dash(t.Message))
		}
		return tw.Flush()
	}
	ref := rest[1]
	name := target
	if ts, err := a.Resolve([]string{target}); err == nil && len(ts) == 1 {
		name = ts[0].Name
	}
	if !o.wait {
		r, err := oneRead(ctx, a, target, func(ctx context.Context, b bmc.Backend) (any, error) { return b.Task(ctx, ref) })
		if err != nil {
			return err
		}
		if o.json {
			return emit(stdout, r)
		}
		t := r.Data.(*bmc.Task)
		fmt.Fprintf(stdout, "%s  %s  %s  %s  (%s, via %s)\n", r.Target, t.ID, t.State, pctLabel(t), kindLabel(t.Kind), r.Via)
		row := func(k, v string) {
			if v != "" {
				fmt.Fprintf(stdout, "   %-8s %s\n", k, v)
			}
		}
		row("name", t.Name)
		if t.Health != "" && t.Health != "OK" {
			row("health", t.Health)
		}
		for i, m := range t.Messages {
			if i == 0 {
				row("message", m)
			} else {
				row("", m)
			}
		}
		if len(t.Messages) == 0 {
			row("message", t.Message)
		}
		row("started", t.Start)
		row("ended", t.End)
		if d := t.Duration(); d > 0 {
			row("took", fmtDur(d))
		}
		row("uri", t.URI)
		switch {
		case !t.Done():
			row("follow", fmt.Sprintf("%s task %s %s --wait", self(o), name, bmc.ShortRef(firstNonEmpty(t.URI, ref))))
		case t.Phase == bmc.PhaseScheduled:
			row("next", "staged: the BMC applies it at the next host reboot")
		}
		return nil
	}
	w, pr, err := watchOpts(o, stderr, name, ref)
	if err != nil {
		return err
	}
	out, err := a.WatchTask(ctx, target, ref, w)
	if pr != nil {
		pr.end()
	}
	if err != nil {
		return err
	}
	if o.json {
		if err := emit(stdout, out); err != nil {
			return err
		}
		return codeErr(outcomeCode(out))
	}
	return codeErr(verdict(o, stdout, stderr, out))
}

func outcomeCode(out *bmc.Outcome) int {
	switch {
	case out.Ok():
		return 0
	case out.Verdict == bmc.VerdictInterrupted:
		return exitInterrupted
	}
	return exitTaskNotDone
}

func codeErr(code int) error {
	if code == 0 {
		return nil
	}
	return &exitErr{code: code}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
