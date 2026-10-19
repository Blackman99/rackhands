package bmc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Blackman99/rackhands/internal/redfish"
)

// Task phases: one vocabulary over DMTF TaskState / JobState, Dell JobState
// and the iLO flash state. Everything but PhaseRunning is final for a watch.
const (
	PhaseRunning   = "running"
	PhaseScheduled = "scheduled" // staged; the BMC applies it at the next host reboot (iDRAC "Scheduled")
	PhaseCompleted = "completed"
	PhaseFailed    = "failed"
	PhaseCancelled = "cancelled"
	PhaseAttention = "needs-attention" // DMTF Job "UserIntervention"
)

// Watch verdicts that are not a task phase.
const (
	VerdictTimeout     = "timeout"     // deadline passed; the task keeps running on the BMC
	VerdictInterrupted = "interrupted" // Ctrl-C / caller cancelled the watch, not the task
	VerdictVanished    = "vanished"    // the BMC dropped the task while we watched it
	VerdictNoTask      = "no-task"     // the request was accepted but the BMC returned nothing to watch
)

// Task kinds.
const (
	KindTask     = "task"      // DMTF TaskService task (or task monitor)
	KindJob      = "job"       // DMTF Job
	KindIDRACJob = "idrac-job" // Dell iDRAC job (DellJob, or a Task carrying Oem.Dell)
	KindILOFlash = "ilo-flash" // HPE iLO 5 UpdateService Oem.Hpe flash state
)

// ILOFlash is the pseudo task id for the iLO flash state.
const ILOFlash = "ilo-flash"

// ErrTaskNotFound means the BMC has no such task (or no longer has it).
var ErrTaskNotFound = errors.New("task not found")

// Task is one normalised task, job or flash state.
type Task struct {
	ID       string   `json:"id"`
	URI      string   `json:"uri,omitempty"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name,omitempty"`
	State    string   `json:"state"` // as the BMC spells it: Running, Exception, Scheduled, Writing …
	Phase    string   `json:"phase"`
	Health   string   `json:"health,omitempty"` // TaskStatus / JobStatus
	Percent  *int     `json:"percent_complete,omitempty"`
	Message  string   `json:"message,omitempty"` // the message that explains the state best
	Messages []string `json:"messages,omitempty"`
	Start    string   `json:"start_time,omitempty"`
	End      string   `json:"end_time,omitempty"`

	retryAfter time.Duration
}

// Done reports whether the task reached a final phase.
func (t *Task) Done() bool { return t != nil && t.Phase != PhaseRunning }

// Duration is EndTime-StartTime as the BMC reports them (0 if unknown).
func (t *Task) Duration() time.Duration {
	s, err1 := time.Parse(time.RFC3339, t.Start)
	e, err2 := time.Parse(time.RFC3339, t.End)
	if err1 != nil || err2 != nil || e.Before(s) {
		return 0
	}
	return e.Sub(s)
}

// ShortRef is how a person types the task back: "JID_001" or "1" for
// members of a task/job collection, the full URI for anything else (monitors).
func ShortRef(uri string) string {
	for _, coll := range []string{"/TaskService/Tasks/", "/Jobs/"} {
		if i := strings.LastIndex(uri, coll); i >= 0 {
			if id := strings.Trim(uri[i+len(coll):], "/"); id != "" && !strings.Contains(id, "/") {
				return id
			}
		}
	}
	return uri
}

func pct(o redfish.Object, key string) *int {
	if v, ok := o.Num(key); ok {
		n := int(v)
		if n < 0 {
			n = 0
		}
		if n > 100 {
			n = 100
		}
		return &n
	}
	return nil
}

// taskTime keeps real timestamps only (iDRAC says TIME_NOW / TIME_NA).
func taskTime(s string) string {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-0700", "2006-01-02T15:04Z07:00", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// TaskPhase maps a DMTF TaskState / JobState (DSP0268 Task, Job).
func TaskPhase(state, health string) string {
	switch strings.ToLower(state) {
	case "completed":
		if NormHealth(health) == Critical {
			return PhaseFailed
		}
		return PhaseCompleted
	case "exception", "killed":
		return PhaseFailed
	case "cancelled":
		return PhaseCancelled
	case "userintervention":
		return PhaseAttention
	}
	// New, Starting, Running, Pending, Suspended, Interrupted, Stopping,
	// Cancelling, Service, Continue, or something new: keep watching.
	return PhaseRunning
}

// DellJobPhase maps iDRAC JobState (DellJob).
func DellJobPhase(state string) string {
	switch strings.ToLower(state) {
	case "completed", "rebootcompleted":
		return PhaseCompleted
	case "failed", "completedwitherrors", "rebootfailed":
		return PhaseFailed
	case "scheduled", "rebootpending", "pendingactivation":
		return PhaseScheduled
	}
	// New, Downloading, Downloaded, Scheduling, ReadyForExecution, Waiting, Running, Paused …
	return PhaseRunning
}

// ILOFlashPhase maps iLO 5 UpdateService Oem.Hpe.State.
func ILOFlashPhase(state string) string {
	switch strings.ToLower(state) {
	case "complete":
		return PhaseCompleted
	case "error":
		return PhaseFailed
	}
	return PhaseRunning // Idle (not started yet), Uploading, Progressing, Verifying, Writing, Updating …
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 2
	case "warning":
		return 1
	}
	return 0
}

// messages collects Messages[] and picks the one that explains the state:
// the most severe, the latest among equals, and a generic TaskEvent
// lifecycle message ("The task with Id '2' has been aborted.") only when
// there is nothing more specific (the Update.VerificationFailed next to it).
func messages(o redfish.Object) (all []string, best string) {
	bestRank := -1
	for _, m := range o.List("Messages") {
		text := m.Str("Message")
		if text == "" {
			text = strings.TrimSpace(m.Str("MessageId") + " " + strings.Join(m.Strings("MessageArgs"), ", "))
		}
		if text == "" {
			continue
		}
		all = append(all, text)
		r := 2 * severityRank(first(m, "MessageSeverity", "Severity"))
		if !strings.HasPrefix(m.Str("MessageId"), "TaskEvent.") {
			r++
		}
		if r >= bestRank {
			bestRank, best = r, text
		}
	}
	return all, best
}

// ParseTask normalises a Task, Job or DellJob resource. uri is where it was
// read (the resource's own @odata.id wins: a task monitor answers with the
// Task it watches).
func ParseTask(o redfish.Object, uri string) *Task {
	t := &Task{ID: o.Str("Id"), URI: uri, Name: o.Str("Name"), Start: taskTime(o.Str("StartTime")), End: taskTime(first(o, "EndTime", "CompletionTime"))}
	if id := o.ID(); id != "" {
		t.URI = id
	}
	if t.ID == "" {
		t.ID = ShortRef(t.URI)
	}
	t.Messages, t.Message = messages(o)
	t.Percent = pct(o, "PercentComplete")
	switch {
	case o.Str("TaskState") != "":
		t.Kind, t.State, t.Health = KindTask, o.Str("TaskState"), NormHealth(o.Str("TaskStatus"))
		t.Phase = TaskPhase(t.State, t.Health)
	case strings.Contains(o.Str("@odata.type"), "DellJob"):
		t.Kind, t.State = KindIDRACJob, o.Str("JobState")
		t.Phase = DellJobPhase(t.State)
		if m := o.Str("Message"); m != "" {
			t.Message, t.Messages = m, []string{m}
		}
	default:
		t.Kind, t.State, t.Health = KindJob, o.Str("JobState"), NormHealth(o.Str("JobStatus"))
		t.Phase = TaskPhase(t.State, t.Health)
	}
	// iDRAC: the Task carries the job; its JobState is the truth (TaskState
	// stays "Running" while a BIOS job sits Scheduled until the reboot).
	if dj := o.Obj("Oem.Dell"); dj.Str("JobState") != "" {
		t.Kind, t.State, t.Phase = KindIDRACJob, dj.Str("JobState"), DellJobPhase(dj.Str("JobState"))
		if p := pct(dj, "PercentComplete"); p != nil {
			t.Percent = p
		}
		if n := dj.Str("Name"); n != "" && (t.Name == "" || t.Name == "Task") {
			t.Name = n
		}
		if m := dj.Str("Message"); m != "" {
			t.Message = m
			if len(t.Messages) == 0 || t.Messages[0] != m {
				t.Messages = append([]string{m}, t.Messages...)
			}
		}
	}
	if t.Phase == PhaseFailed && t.Health == "" {
		t.Health = Critical
	}
	// A task can end Completed with TaskStatus OK and still carry a warning
	// ("The property SKU is a read only property"): surface it.
	if t.Phase == PhaseCompleted {
		for _, m := range o.List("Messages") {
			if sev := first(m, "MessageSeverity", "Severity"); severityRank(sev) > severityRank(t.Health) {
				t.Health = NormHealth(sev)
			}
		}
	}
	if t.State == "" {
		t.State = "Unknown"
	}
	return t
}

func iloFlash(us redfish.Object) (*Task, bool) {
	for _, k := range []string{"Oem.Hpe", "Oem.Hp"} {
		o := us.Obj(k)
		st := o.Str("State")
		if st == "" {
			continue
		}
		t := &Task{ID: ILOFlash, URI: us.ID(), Kind: KindILOFlash, Name: "iLO firmware flash", State: st, Phase: ILOFlashPhase(st), Percent: pct(o, "FlashProgressPercent")}
		if r := o.Obj("Result"); r.Str("Message") != "" || r.Str("MessageId") != "" {
			t.Message = r.Str("Message")
			if t.Message == "" {
				t.Message = humanID(r.Str("MessageId"))
				if args := r.Strings("MessageArgs"); len(args) > 0 {
					t.Message += ": " + strings.Join(args, ", ")
				}
			}
			t.Messages = []string{t.Message}
		}
		if t.Phase == PhaseFailed {
			t.Health = Critical
		}
		return t, true
	}
	return nil, false
}

// humanID turns a registry MessageId such as "Update.1.1.UpdateSuccessful"
// into "Update successful" for BMCs that send ids without the text.
func humanID(id string) string {
	key := id[strings.LastIndex(id, ".")+1:]
	var b strings.Builder
	for i, c := range key {
		if i > 0 && c >= 'A' && c <= 'Z' {
			b.WriteByte(' ')
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	if b.Len() == 0 {
		return id
	}
	return b.String()
}

// --- Redfish backend

func (r *Redfish) tasksPath(ctx context.Context) string {
	if ts := r.root.Ref("TaskService"); ts != "" {
		if svc, err := r.c.Get(ctx, ts); err == nil && svc.Ref("Tasks") != "" {
			return svc.Ref("Tasks")
		}
	}
	return "/redfish/v1/TaskService/Tasks"
}

func (r *Redfish) dellJobsPath() string {
	if r.prof.Vendor != "Dell" || r.manager.ID() == "" {
		return ""
	}
	return strings.TrimRight(r.manager.ID(), "/") + "/Jobs"
}

// Tasks implements Backend: the TaskService tasks, the iDRAC job queue on
// Dell, the iLO flash state on HPE. Unfinished first, newest 50 per source.
func (r *Redfish) Tasks(ctx context.Context) ([]Task, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	out := []Task{}
	seen := map[string]bool{}
	found := false
	add := func(objs []redfish.Object) {
		for i := len(objs) - 1; i >= 0; i-- { // newest first
			t := ParseTask(objs[i], "")
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			out = append(out, *t)
		}
	}
	if p := r.dellJobsPath(); p != "" {
		if js, err := r.c.MembersN(ctx, p, 50); err == nil {
			found = true
			add(js)
		} else if errors.Is(err, redfish.ErrUnreachable) {
			return nil, err
		}
	}
	if r.root.Ref("TaskService") != "" {
		ts, err := r.c.MembersN(ctx, r.tasksPath(ctx), 50)
		if errors.Is(err, redfish.ErrUnreachable) {
			return nil, err
		}
		if err == nil {
			found = true
			add(ts)
		}
	}
	if r.prof.Vendor == "HPE" {
		if us := r.root.Ref("UpdateService"); us != "" {
			if u, err := r.c.Get(ctx, us); err == nil {
				if t, ok := iloFlash(u); ok {
					found = true
					out = append([]Task{*t}, out...)
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s exposes no TaskService", ErrUnsupported, r.bmcLabel())
	}
	rank := func(t *Task) int {
		switch t.Phase {
		case PhaseRunning:
			return 0
		case PhaseScheduled, PhaseAttention:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(&out[i]) < rank(&out[j]) })
	return out, nil
}

// Task implements Backend. ref is an id from Tasks ("JID_001", "7",
// "ilo-flash") or a URI (a Task, Job or DSP0266 task monitor).
func (r *Redfish) Task(ctx context.Context, ref string) (*Task, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("which task? give an id from `rackhands task TARGET` or a task URI")
	}
	if ref == ILOFlash {
		us := r.root.Ref("UpdateService")
		if us == "" {
			return nil, fmt.Errorf("%w: no UpdateService", ErrUnsupported)
		}
		u, err := r.c.Get(ctx, us)
		if err != nil {
			return nil, err
		}
		t, ok := iloFlash(u)
		if !ok {
			return nil, fmt.Errorf("%w: this BMC reports no iLO flash state (UpdateService Oem.Hpe.State)", ErrUnsupported)
		}
		return t, nil
	}
	var cands []string
	if strings.HasPrefix(ref, "/") || strings.Contains(ref, "://") {
		cands = []string{ref}
	} else {
		if p := r.dellJobsPath(); p != "" {
			cands = append(cands, p+"/"+ref)
		}
		cands = append(cands, strings.TrimRight(r.tasksPath(ctx), "/")+"/"+ref)
	}
	var err error
	for _, c := range cands {
		var t *Task
		if t, err = r.fetchTask(ctx, c); err == nil {
			return t, nil
		}
		if !errors.Is(err, ErrTaskNotFound) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: no %s on this BMC; list them with `rackhands task %s`", ErrTaskNotFound, ref, r.target)
}

// fetchTask reads a task, job or task monitor. Per DSP0266 a monitor answers
// 202 (+ the Task) while the operation runs and the operation's own final
// response once it is done.
func (r *Redfish) fetchTask(ctx context.Context, uri string) (*Task, error) {
	resp, err := r.c.Do(ctx, http.MethodGet, uri, nil, nil)
	if err != nil {
		if redfish.IsStatus(err, http.StatusNotFound) || redfish.IsStatus(err, http.StatusGone) {
			return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, uri)
		}
		return nil, err
	}
	var o redfish.Object
	_ = json.Unmarshal(resp.Body, &o)
	var t *Task
	switch {
	case o.Str("TaskState") != "" || o.Str("JobState") != "":
		t = ParseTask(o, uri)
	case resp.Status == http.StatusAccepted:
		t = &Task{ID: ShortRef(uri), URI: uri, Kind: KindTask, State: "Running", Phase: PhaseRunning}
	default: // the monitor handed back the operation's result
		t = &Task{ID: ShortRef(uri), URI: uri, Kind: KindTask, State: "Completed", Phase: PhaseCompleted,
			Message: redfish.ExtendedMessage(resp.Body)}
		if t.Message == "" {
			t.Message = fmt.Sprintf("the task monitor returned HTTP %d", resp.Status)
		}
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		t.retryAfter = time.Duration(s) * time.Second
	}
	return t, nil
}

// taskRef finds what to watch after an action: the Location header (task
// or task monitor) or a Task in the response body.
func taskRef(resp *redfish.Response) string {
	if resp == nil {
		return ""
	}
	if resp.Location != "" {
		return resp.Location
	}
	var o redfish.Object
	if json.Unmarshal(resp.Body, &o) == nil && o.Str("TaskState") != "" {
		return o.ID()
	}
	return ""
}

// --- watching

// TaskUpdate is one observation during a watch.
type TaskUpdate struct {
	Task    *Task         // latest snapshot (nil until the first answer)
	Err     error         // set while the BMC is not answering and we retry
	Waiting bool          // still the state seen before the request: the BMC has not started yet
	Elapsed time.Duration // since the watch started
	Polls   int
}

// WatchOptions bound a watch. Zero values mean the defaults.
type WatchOptions struct {
	Timeout  time.Duration // overall deadline (default 30m)
	Every    time.Duration // first poll interval, and the interval after every change (default 2s)
	MaxEvery time.Duration // backoff cap (default 15s)
	// Before is the snapshot taken before the request (iLO keeps showing the
	// previous flash as Complete). Until the state moves away from it, or
	// Grace passes, it is not taken as this request's outcome.
	Before   *Task
	Grace    time.Duration // default 2m
	OnUpdate func(TaskUpdate)
}

// Outcome is how a watch ended.
type Outcome struct {
	Target   string  `json:"target"`
	Ref      string  `json:"task_ref"`
	Verdict  string  `json:"verdict"` // a final phase, or timeout / interrupted / vanished / no-task
	Task     *Task   `json:"task,omitempty"`
	Elapsed  float64 `json:"elapsed_s"`
	Polls    int     `json:"polls"`
	Deadline float64 `json:"deadline_s,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// Ok reports whether the outcome is a success (completed, staged for the
// next reboot, or nothing to watch).
func (o *Outcome) Ok() bool {
	return o.Verdict == PhaseCompleted || o.Verdict == PhaseScheduled || o.Verdict == VerdictNoTask
}

// Transient is an error worth retrying while watching: the BMC is down or
// busy (it often restarts to apply its own firmware).
func Transient(err error) bool {
	if errors.Is(err, redfish.ErrUnreachable) {
		return true
	}
	for _, s := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if redfish.IsStatus(err, s) {
			return true
		}
	}
	return false
}

func sameTask(a, b *Task) bool {
	if a == nil || b == nil {
		return a == b
	}
	pa, pb := -1, -1
	if a.Percent != nil {
		pa = *a.Percent
	}
	if b.Percent != nil {
		pb = *b.Percent
	}
	return a.State == b.State && pa == pb && a.Message == b.Message
}

// Watch polls one task until it reaches a final phase, the deadline passes
// or ctx is cancelled. It only ever sends GETs. Polling backs off from Every
// to MaxEvery while nothing changes, honours Retry-After, and rides out a BMC
// that stops answering (a restart to apply its own firmware) until the deadline.
func Watch(ctx context.Context, b Backend, target, ref string, o WatchOptions) (*Outcome, error) {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Every <= 0 {
		o.Every = 2 * time.Second
	}
	if o.MaxEvery <= 0 {
		o.MaxEvery = 15 * time.Second
	}
	if o.MaxEvery < o.Every {
		o.MaxEvery = o.Every
	}
	if o.Grace <= 0 {
		o.Grace = 2 * time.Minute
	}
	start := time.Now()
	out := &Outcome{Target: target, Ref: ref, Deadline: o.Timeout.Seconds()}
	finish := func(v string, t *Task) (*Outcome, error) {
		out.Verdict, out.Task = v, t
		out.Elapsed = time.Since(start).Round(time.Millisecond).Seconds()
		return out, nil
	}
	dctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	every := o.Every
	var last *Task
	for {
		t, err := b.Task(dctx, ref)
		out.Polls++
		upd := TaskUpdate{Elapsed: time.Since(start), Polls: out.Polls}
		switch {
		case err == nil:
			if t.Kind != KindILOFlash && t.URI != "" && t.URI != ref {
				ref = t.URI // a task monitor pointed at its Task: follow the Task, monitors may vanish
				out.Ref = ref
			}
			stale := o.Before != nil && sameTask(t, o.Before) && time.Since(start) < o.Grace
			if t.Done() && !stale {
				if o.Before != nil && sameTask(t, o.Before) {
					out.Note = fmt.Sprintf("the BMC already reported %s before this request and never showed it running; check the installed version with `rackhands firmware %s`", t.State, target)
				}
				return finish(t.Phase, t)
			}
			if o.Before != nil && !sameTask(t, o.Before) {
				o.Before = nil // it moved: from now on every state is this request's
			}
			if !sameTask(last, t) {
				every = o.Every
			}
			last = t
			upd.Task, upd.Waiting = t, stale
			if t.retryAfter > 0 {
				every = min(max(t.retryAfter, o.Every), time.Minute)
			}
		case dctx.Err() != nil:
			// deadline or Ctrl-C while the request was in flight
		case Transient(err):
			upd.Task, upd.Err = last, err
		case errors.Is(err, ErrTaskNotFound) && last != nil:
			out.Note = "the BMC dropped the task while it was being watched (it may have restarted); check the result with `rackhands firmware " + target + "`"
			return finish(VerdictVanished, last)
		default:
			return nil, err
		}
		if o.OnUpdate != nil && dctx.Err() == nil {
			o.OnUpdate(upd)
		}
		select {
		case <-dctx.Done():
			if ctx.Err() != nil {
				return finish(VerdictInterrupted, last)
			}
			return finish(VerdictTimeout, last)
		case <-time.After(every):
		}
		every = min(every*3/2, o.MaxEvery)
	}
}
