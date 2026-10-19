package bmc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Blackman99/rackhands/internal/redfish"
)

var fast = WatchOptions{Every: time.Millisecond, MaxEvery: 2 * time.Millisecond, Timeout: 5 * time.Second}

func TestTaskParsingPerVendor(t *testing.T) {
	cases := []struct {
		vendor, ref, kind, state, phase, msg string
		pct                                  int
	}{
		// iDRAC job queue: DellJob, JobState is the truth, TIME_NOW is not a time
		{"dell", "JID_003", KindIDRACJob, "Scheduled", PhaseScheduled, "Task successfully scheduled.", 0},
		{"dell", "JID_001", KindIDRACJob, "Completed", PhaseCompleted, "Job completed successfully.", 100},
		// the Task iDRAC points SimpleUpdate at: TaskState Exception + Oem.Dell JobState Failed
		{"dell", "/redfish/v1/TaskService/Tasks/JID_002", KindIDRACJob, "Failed", PhaseFailed, "Unable to verify Update Package signature.", 0},
		// TaskState says Running, the job says Scheduled: the job wins
		{"dell", "/redfish/v1/TaskService/Tasks/JID_003", KindIDRACJob, "Scheduled", PhaseScheduled, "Task successfully scheduled.", 0},
		// DMTF Exception: the specific Update message beats the generic TaskEvent one
		{"lenovo", "2", KindTask, "Exception", PhaseFailed, "Verification of image 'lnvgy_fw_xcc_uhc832d-2.20.uxz' at 'BMC-Primary' failed.", 12},
	}
	for _, c := range cases {
		t.Run(c.vendor+" "+c.ref, func(t *testing.T) {
			b, _ := backend(t, c.vendor)
			tk, err := b.Task(ctx, c.ref)
			if err != nil {
				t.Fatal(err)
			}
			if tk.Kind != c.kind || tk.State != c.state || tk.Phase != c.phase || tk.Message != c.msg || tk.Percent == nil || *tk.Percent != c.pct {
				t.Fatalf("got %+v (pct %v)", tk, pctStr(tk))
			}
		})
	}
	b, _ := backend(t, "dell")
	tk, _ := b.Task(ctx, "JID_003")
	if tk.Start != "" || tk.End != "" {
		t.Fatalf("TIME_NOW/TIME_NA must not pass as timestamps: %q %q", tk.Start, tk.End)
	}
	if tk.Done() != true {
		t.Fatal("scheduled ends a watch")
	}
	l, _ := backend(t, "lenovo")
	done, _ := l.Task(ctx, "2")
	if done.Duration() != 41*time.Second || done.Health != Critical {
		t.Fatalf("duration/health from StartTime/EndTime: %v %s", done.Duration(), done.Health)
	}
}

func pctStr(t *Task) string {
	if t == nil || t.Percent == nil {
		return "nil"
	}
	return strings.Repeat("|", *t.Percent/10)
}

func TestTaskMonitorPointsAtTask(t *testing.T) {
	b, _ := backend(t, "lenovo")
	tk, err := b.Task(ctx, "/redfish/v1/TaskService/TaskMonitors/1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.URI != "/redfish/v1/TaskService/Tasks/1" || tk.Phase != PhaseRunning || tk.ID != "1" {
		t.Fatalf("monitor: %+v", tk)
	}
}

func TestTaskNotFoundAndUnsupported(t *testing.T) {
	b, _ := backend(t, "dell")
	if _, err := b.Task(ctx, "JID_999"); !errors.Is(err, ErrTaskNotFound) || !strings.Contains(err.Error(), "no JID_999 on this BMC; list them with `rackhands task dell-box`") {
		t.Fatalf("want a not-found error that says how to list tasks, got %v", err)
	}
	s, _ := backend(t, "supermicro")
	if _, err := s.Tasks(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("no TaskService: %v", err)
	}
	if _, err := (&IPMI{}).Task(ctx, "1"); !errors.Is(err, ErrUnsupported) {
		t.Fatal("ipmi")
	}
}

func TestTasksList(t *testing.T) {
	d, _ := backend(t, "dell")
	ts, err := d.Tasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range ts {
		ids = append(ids, x.ID+":"+x.Phase)
	}
	// job queue + TaskService deduplicated by JID; unfinished first, newest first
	if got := strings.Join(ids, " "); got != "JID_003:scheduled JID_002:failed JID_001:completed" {
		t.Fatalf("dell tasks: %s", got)
	}
	h, _ := backend(t, "hpe")
	ts, err = h.Tasks(ctx)
	if err != nil || len(ts) != 1 || ts[0].ID != ILOFlash || ts[0].Kind != KindILOFlash {
		t.Fatalf("hpe tasks: %v %+v", err, ts)
	}
}

// The firmware executor reports what to watch, per vendor, and the watch
// follows the mockup's task through its steps to the end.
func TestFirmwareUpdateThenWatch(t *testing.T) {
	cases := []struct{ vendor, ref, id, final, msg string }{
		{"dell", "/redfish/v1/TaskService/Tasks/JID_001", "JID_001", "Completed", "Job completed successfully."},
		{"lenovo", "/redfish/v1/TaskService/TaskMonitors/1", "1", "Completed", "The target device 'UEFI' has been successfully updated with image 'lnvgy_fw_uefi_afe130k-1.90.uxz'."},
		{"hpe", ILOFlash, ILOFlash, "Complete", "Update successful: System ROM, U46_2.90_02_20_2026.fwpkg"},
	}
	for _, c := range cases {
		t.Run(c.vendor, func(t *testing.T) {
			b, _ := backend(t, c.vendor)
			p, err := b.PlanFirmwareUpdate(ctx, "https://repo.example/fw.bin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Execute(ctx); err != nil {
				t.Fatal(err)
			}
			ref, before := p.Task()
			if ref != c.ref {
				t.Fatalf("task ref %q", ref)
			}
			var seen []string
			o := fast
			o.Before = before
			o.OnUpdate = func(u TaskUpdate) {
				if u.Task != nil {
					seen = append(seen, u.Task.State+" "+pctStr(u.Task))
				}
			}
			out, err := Watch(ctx, b, "box", ref, o)
			if err != nil {
				t.Fatal(err)
			}
			if out.Verdict != PhaseCompleted || out.Task.ID != c.id || out.Task.State != c.final || out.Task.Message != c.msg || len(seen) < 3 {
				t.Fatalf("outcome %+v task %+v progress %v", out, out.Task, seen)
			}
			if c.vendor == "lenovo" && out.Ref != "/redfish/v1/TaskService/Tasks/1" {
				t.Fatalf("watch should leave the monitor for the Task: %s", out.Ref)
			}
		})
	}
}

func TestWatchRidesOutBMCRestart(t *testing.T) {
	b, s := backend(t, "dell")
	s.SetOutages(3)
	var errs int
	o := fast
	o.OnUpdate = func(u TaskUpdate) {
		if u.Err != nil {
			errs++
		}
	}
	out, err := Watch(ctx, b, "box", "/redfish/v1/TaskService/Tasks/JID_001", o)
	if err != nil || out.Verdict != PhaseCompleted || errs != 3 {
		t.Fatalf("%v %+v errs=%d", err, out, errs)
	}
}

// script is a Backend whose Task answers come from a list.
type script struct {
	Backend
	steps []func() (*Task, error)
	n     int
}

func (s *script) Task(context.Context, string) (*Task, error) {
	f := s.steps[min(s.n, len(s.steps)-1)]
	s.n++
	return f()
}

func running(p int) func() (*Task, error) {
	return func() (*Task, error) { return &Task{ID: "7", State: "Running", Phase: PhaseRunning, Percent: &p}, nil }
}

func TestWatchEndings(t *testing.T) {
	notFound := func() (*Task, error) { return nil, ErrTaskNotFound }
	unauthorized := func() (*Task, error) { return nil, &redfish.HTTPError{Status: http.StatusUnauthorized} }
	busy := func() (*Task, error) { return nil, &redfish.HTTPError{Status: http.StatusServiceUnavailable} }

	out, err := Watch(ctx, &script{steps: []func() (*Task, error){running(10), busy, running(60), notFound}}, "x", "7", fast)
	if err != nil || out.Verdict != VerdictVanished || *out.Task.Percent != 60 {
		t.Fatalf("vanished: %v %+v", err, out)
	}
	if _, err := Watch(ctx, &script{steps: []func() (*Task, error){notFound}}, "x", "7", fast); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task must be an error, not a verdict: %v", err)
	}
	if _, err := Watch(ctx, &script{steps: []func() (*Task, error){running(1), unauthorized}}, "x", "7", fast); !redfish.IsStatus(err, 401) {
		t.Fatalf("bad credentials must stop the watch: %v", err)
	}
	o := fast
	o.Timeout = 30 * time.Millisecond
	start := time.Now()
	out, err = Watch(ctx, &script{steps: []func() (*Task, error){running(40)}}, "x", "7", o)
	if err != nil || out.Verdict != VerdictTimeout || time.Since(start) > time.Second || out.Polls < 3 {
		t.Fatalf("timeout: %v %+v", err, out)
	}
	cctx, cancel := context.WithCancel(ctx)
	o = fast
	o.OnUpdate = func(u TaskUpdate) {
		if u.Polls == 2 {
			cancel() // Ctrl-C
		}
	}
	out, err = Watch(cctx, &script{steps: []func() (*Task, error){running(40)}}, "x", "7", o)
	if err != nil || out.Verdict != VerdictInterrupted || out.Ok() {
		t.Fatalf("interrupted: %v %+v", err, out)
	}
}

func TestWatchBackoffAndRetryAfter(t *testing.T) {
	var at []time.Time
	o := WatchOptions{Every: 10 * time.Millisecond, MaxEvery: 25 * time.Millisecond, Timeout: 5 * time.Second,
		OnUpdate: func(TaskUpdate) { at = append(at, time.Now()) }}
	steps := []func() (*Task, error){running(5), running(5), running(5), running(5), running(5),
		func() (*Task, error) { return &Task{State: "Completed", Phase: PhaseCompleted}, nil }}
	if _, err := Watch(ctx, &script{steps: steps}, "x", "7", o); err != nil {
		t.Fatal(err)
	}
	if len(at) != 5 {
		t.Fatalf("%d updates", len(at))
	}
	if g1, g4 := at[1].Sub(at[0]), at[4].Sub(at[3]); g4 <= g1 || g4 > 200*time.Millisecond {
		t.Fatalf("no backoff while nothing changes: first gap %v, last gap %v", g1, g4)
	}
	ra := &Task{State: "Running", Phase: PhaseRunning, retryAfter: time.Second}
	o = WatchOptions{Every: time.Millisecond, Timeout: 300 * time.Millisecond}
	out, _ := Watch(ctx, &script{steps: []func() (*Task, error){func() (*Task, error) { return ra, nil }}}, "x", "7", o)
	if out.Polls != 1 {
		t.Fatalf("Retry-After: 1 must slow polling down, got %d polls in 300ms", out.Polls)
	}
}

func TestWatchIgnoresStateFromBeforeTheRequest(t *testing.T) {
	prev := &Task{ID: ILOFlash, Kind: KindILOFlash, State: "Complete", Phase: PhaseCompleted}
	complete := func() (*Task, error) {
		return &Task{ID: ILOFlash, Kind: KindILOFlash, State: "Complete", Phase: PhaseCompleted}, nil
	}
	steps := []func() (*Task, error){complete, complete, running(30), complete}
	o := fast
	o.Before = prev
	out, err := Watch(ctx, &script{steps: steps}, "x", ILOFlash, o)
	if err != nil || out.Verdict != PhaseCompleted || out.Polls != 4 || out.Note != "" {
		t.Fatalf("the previous run's Complete must not end the watch: %v %+v", err, out)
	}
	o.Grace = 5 * time.Millisecond
	out, _ = Watch(ctx, &script{steps: []func() (*Task, error){complete}}, "x", ILOFlash, o)
	if out.Verdict != PhaseCompleted || !strings.Contains(out.Note, "before this request") {
		t.Fatalf("after the grace period, say we never saw it run: %+v", out)
	}
}

func TestShortRef(t *testing.T) {
	for in, want := range map[string]string{
		"/redfish/v1/TaskService/Tasks/JID_001":            "JID_001",
		"/redfish/v1/Managers/iDRAC.Embedded.1/Jobs/JID_7": "JID_7",
		"/redfish/v1/TaskService/TaskMonitors/1":           "/redfish/v1/TaskService/TaskMonitors/1",
		"https://bmc/redfish/v1/TaskService/Tasks/12/":     "12",
	} {
		if got := ShortRef(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestCompletedTaskSurfacesWarningMessages(t *testing.T) {
	var o redfish.Object
	if err := json.Unmarshal([]byte(`{"Id":"545","TaskState":"Completed","TaskStatus":"OK","Messages":[{"MessageId":"Base.1.0.PropertyNotWritable","Message":"The property SKU is a read only property","Severity":"Warning"}]}`), &o); err != nil {
		t.Fatal(err)
	}
	got := ParseTask(o, "/redfish/v1/TaskService/Tasks/545")
	if got.Phase != PhaseCompleted || got.Health != Warning || got.Message != "The property SKU is a read only property" {
		t.Fatalf("%+v", got)
	}
}
