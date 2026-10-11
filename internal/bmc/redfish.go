package bmc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Blackman99/rackhands/internal/redfish"
)

// Redfish is the Redfish backend.
type Redfish struct {
	c      *redfish.Client
	target string
	force  string // vendor override from config ("" = detect)

	once    sync.Once
	loadErr error
	root    redfish.Object
	system  redfish.Object
	manager redfish.Object
	chassis []redfish.Object
	prof    Profile
}

// NewRedfish wraps a client. vendor may be "" or "auto" to detect.
func NewRedfish(c *redfish.Client, target, vendor string) *Redfish {
	if strings.EqualFold(vendor, "auto") {
		vendor = ""
	}
	return &Redfish{c: c, target: target, force: vendor}
}

func (r *Redfish) Protocol() string { return "redfish" }

func (r *Redfish) Close(ctx context.Context) { r.c.Close(ctx) }

// Probe checks that a Redfish service answers at all.
func (r *Redfish) Probe(ctx context.Context) error {
	r.load(ctx)
	return r.loadErr
}

func (r *Redfish) load(ctx context.Context) {
	r.once.Do(func() {
		root, err := r.c.Get(ctx, "/redfish/v1/")
		if err != nil {
			if redfish.IsStatus(err, http.StatusNotFound) {
				err = fmt.Errorf("%w: no /redfish/v1 on %s", redfish.ErrUnreachable, r.c.Host())
			}
			r.loadErr = err
			return
		}
		r.root = root
		if ref := root.Ref("Systems"); ref != "" {
			sys, err := r.c.Members(ctx, ref)
			if err != nil {
				r.loadErr = err
				return
			}
			if len(sys) > 0 {
				r.system = sys[0]
			}
		}
		if r.system == nil {
			r.system = redfish.Object{}
		}
		mgrRef := ""
		if refs := r.system.Refs("Links.ManagedBy"); len(refs) > 0 {
			mgrRef = refs[0]
		}
		if mgrRef != "" {
			r.manager, _ = r.c.Get(ctx, mgrRef)
		} else if ref := root.Ref("Managers"); ref != "" {
			if ms, err := r.c.Members(ctx, ref); err == nil && len(ms) > 0 {
				r.manager = ms[0]
			}
		}
		if r.manager == nil {
			r.manager = redfish.Object{}
		}
		chRefs := r.system.Refs("Links.Chassis")
		if len(chRefs) == 0 && root.Ref("Chassis") != "" {
			if col, err := r.c.Get(ctx, root.Ref("Chassis")); err == nil {
				chRefs = col.Refs("Members")
			}
		}
		for _, ref := range chRefs {
			if ch, err := r.c.Get(ctx, ref); err == nil {
				r.chassis = append(r.chassis, ch)
			}
		}
		v := r.force
		if v == "" {
			v = DetectVendor(root, r.manager)
		}
		r.prof = ProfileFor(v)
	})
}

func (r *Redfish) ready(ctx context.Context) error {
	r.load(ctx)
	if r.loadErr != nil {
		return r.loadErr
	}
	if r.system.ID() == "" {
		return fmt.Errorf("%s exposes no ComputerSystem", r.c.Host())
	}
	return nil
}

func (r *Redfish) Vendor(ctx context.Context) string {
	r.load(ctx)
	if r.loadErr != nil {
		return ""
	}
	return r.prof.Vendor
}

func (r *Redfish) bmcLabel() string {
	m := r.manager.Str("Model")
	if m == "" {
		m = r.prof.BMC
	}
	if r.prof.Vendor == "HPE" {
		for _, k := range []string{"Oem.Hpe.Manager", "Oem.Hp.Manager"} {
			if l := r.root.List(k); len(l) > 0 && l[0].Str("ManagerType") != "" {
				return l[0].Str("ManagerType")
			}
		}
	}
	return m
}

func health(o redfish.Object) string {
	if h := o.Str("Status.HealthRollup"); h != "" {
		return NormHealth(h)
	}
	return NormHealth(o.Str("Status.Health"))
}

// Inventory implements Backend.
func (r *Redfish) Inventory(ctx context.Context) (*Inventory, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	s := r.system
	inv := &Inventory{
		Target: r.target, Via: "redfish", Vendor: r.prof.Vendor,
		BMC: r.bmcLabel(), BMCFirmware: r.manager.Str("FirmwareVersion"),
		Manufacturer: s.Str("Manufacturer"), Model: s.Str("Model"), Serial: s.Str("SerialNumber"),
		SKU: s.Str("SKU"), BIOS: s.Str("BiosVersion"), PowerState: s.Str("PowerState"),
		Health: health(s), CPUModel: s.Str("ProcessorSummary.Model"), Hostname: s.Str("HostName"),
	}
	if n, ok := s.Num("ProcessorSummary.Count"); ok {
		inv.CPUCount = int(n)
	}
	if n, ok := s.Num("MemorySummary.TotalSystemMemoryGiB"); ok {
		inv.MemoryGiB = n
	}
	if inv.Serial == "" && len(r.chassis) > 0 {
		inv.Serial = r.chassis[0].Str("SerialNumber")
	}
	if r.prof.Vendor == "Dell" {
		if tag := s.Str("SKU"); tag != "" {
			inv.Notes = append(inv.Notes, "Dell service tag "+tag)
		}
	}
	return inv, nil
}

// Health implements Backend.
func (r *Redfish) Health(ctx context.Context) (*HealthReport, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	rep := &HealthReport{Target: r.target, Via: "redfish", Vendor: r.prof.Vendor, Problems: []Problem{}}
	levels := []string{health(r.system)}
	add := func(comp, h, detail string) {
		h = NormHealth(h)
		if h == "" || h == OK {
			return
		}
		for _, p := range rep.Problems {
			if p.Component == comp {
				return
			}
		}
		rep.Problems = append(rep.Problems, Problem{Component: comp, Health: h, Detail: detail})
		levels = append(levels, h)
	}
	for _, ch := range r.chassis {
		levels = append(levels, health(ch))
	}
	levels = append(levels, health(r.manager))
	if h := health(r.manager); h != "" && h != OK {
		add("BMC "+r.bmcLabel(), h, "")
	}
	// Vendor rollups that name the failing subsystem directly.
	switch r.prof.Vendor {
	case "HPE":
		for _, k := range []string{"Oem.Hpe.AggregateHealthStatus", "Oem.Hp.AggregateHealthStatus"} {
			agg := r.system.Obj(k)
			keys := make([]string, 0, len(agg))
			for kk := range agg {
				keys = append(keys, kk)
			}
			sort.Strings(keys)
			for _, kk := range keys {
				sub := agg.Obj(kk)
				add("subsystem "+kk, sub.Str("Status.Health"), "iLO aggregate health")
			}
		}
	case "Dell":
		ds := r.system.Obj("Oem.Dell.DellSystem")
		keys := make([]string, 0, len(ds))
		for kk := range ds {
			if strings.HasSuffix(kk, "RollupStatus") {
				keys = append(keys, kk)
			}
		}
		sort.Strings(keys)
		for _, kk := range keys {
			name := strings.TrimSuffix(kk, "RollupStatus")
			add("subsystem "+dellRollupName(name), ds.Str(kk), "iDRAC rollup")
		}
	}
	if h := NormHealth(r.system.Str("ProcessorSummary.Status.HealthRollup")); h != "" && h != OK {
		r.memberProblems(ctx, r.system.Ref("Processors"), "CPU", add)
	}
	if h := NormHealth(r.system.Str("MemorySummary.Status.HealthRollup")); h != "" && h != OK {
		r.memberProblems(ctx, r.system.Ref("Memory"), "DIMM", add)
	}
	if sensors, err := r.Sensors(ctx); err == nil {
		for _, s := range sensors {
			if s.Health != "" && s.Health != OK {
				d := ""
				if s.Reading != nil {
					d = fmt.Sprintf("reading %g %s", *s.Reading, s.Unit)
				}
				add(s.Kind+" "+s.Name, s.Health, strings.TrimSpace(d))
			}
		}
	}
	rep.Overall = Worst(levels...)
	if rep.Overall == Unknown && len(rep.Problems) == 0 {
		rep.Notes = append(rep.Notes, "BMC reported no health status")
	}
	return rep, nil
}

func dellRollupName(k string) string {
	m := map[string]string{"PS": "power supplies", "Fan": "fans", "Temp": "temperatures", "Volt": "voltages",
		"CPU": "processors", "Storage": "storage", "SysMem": "memory", "Battery": "batteries", "Intrusion": "intrusion",
		"Licensing": "licensing", "SEL": "system event log", "Current": "current", "Cooling": "cooling"}
	if v, ok := m[k]; ok {
		return v
	}
	return k
}

func (r *Redfish) memberProblems(ctx context.Context, ref, label string, add func(string, string, string)) {
	if ref == "" {
		return
	}
	ms, err := r.c.Members(ctx, ref)
	if err != nil {
		return
	}
	for _, m := range ms {
		if strings.EqualFold(m.Str("Status.State"), "Absent") {
			continue
		}
		name := m.Str("Name")
		if name == "" {
			name = m.Str("Id")
		}
		add(label+" "+name, m.Str("Status.Health"), m.Str("Model"))
	}
}

func fptr(o redfish.Object, k string) *float64 {
	if v, ok := o.Num(k); ok {
		return &v
	}
	return nil
}

// thr reads a threshold; iLO and others report 0 for "no threshold".
func thr(o redfish.Object, k string) *float64 {
	if v, ok := o.Num(k); ok && v != 0 {
		return &v
	}
	return nil
}

// unit normalises UCUM and vendor spellings.
func unit(u string) string {
	switch strings.ToLower(u) {
	case "cel", "celsius", "degrees c":
		return "C"
	case "percent", "%":
		return "%"
	case "rpm", "{rev}/min":
		return "RPM"
	case "w", "watts":
		return "W"
	case "v", "volts":
		return "V"
	case "a", "amps":
		return "A"
	}
	return u
}

func first(o redfish.Object, keys ...string) string {
	for _, k := range keys {
		if v := o.Str(k); v != "" {
			return v
		}
	}
	return ""
}

// Sensors implements Backend.
func (r *Redfish) Sensors(ctx context.Context) ([]Sensor, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	var out []Sensor
	absent := func(o redfish.Object) bool { return strings.EqualFold(o.Str("Status.State"), "Absent") }
	for _, ch := range r.chassis {
		got := false
		if ref := ch.Ref("Thermal"); ref != "" {
			if th, err := r.c.Get(ctx, ref); err == nil {
				got = true
				for _, t := range th.List("Temperatures") {
					if absent(t) {
						continue
					}
					out = append(out, Sensor{Name: first(t, "Name", "MemberId"), Kind: "temperature", Reading: fptr(t, "ReadingCelsius"), Unit: "C",
						Health: NormHealth(t.Str("Status.Health")), State: t.Str("Status.State"),
						UpperCrit: thr(t, "UpperThresholdCritical"), LowerCrit: thr(t, "LowerThresholdCritical"), Location: t.Str("PhysicalContext")})
				}
				for _, f := range th.List("Fans") {
					if absent(f) {
						continue
					}
					u := unit(f.Str("ReadingUnits"))
					if u == "" {
						u = "RPM"
					}
					out = append(out, Sensor{Name: first(f, "Name", "FanName", "MemberId"), Kind: "fan", Reading: fptr(f, "Reading"), Unit: u,
						Health: NormHealth(f.Str("Status.Health")), State: f.Str("Status.State"), LowerCrit: thr(f, "LowerThresholdCritical"), Location: f.Str("PhysicalContext")})
				}
			}
		}
		if ref := ch.Ref("Power"); ref != "" {
			if pw, err := r.c.Get(ctx, ref); err == nil {
				got = true
				for _, p := range pw.List("PowerSupplies") {
					if absent(p) {
						continue
					}
					rd := fptr(p, "PowerInputWatts")
					if rd == nil {
						rd = fptr(p, "LastPowerOutputWatts")
					}
					out = append(out, Sensor{Name: first(p, "Name", "MemberId"), Kind: "psu", Reading: rd, Unit: "W",
						Health: NormHealth(p.Str("Status.Health")), State: p.Str("Status.State")})
				}
				for _, v := range pw.List("Voltages") {
					if absent(v) {
						continue
					}
					out = append(out, Sensor{Name: first(v, "Name", "MemberId"), Kind: "voltage", Reading: fptr(v, "ReadingVolts"), Unit: "V",
						Health: NormHealth(v.Str("Status.Health")), State: v.Str("Status.State"),
						UpperCrit: thr(v, "UpperThresholdCritical"), LowerCrit: thr(v, "LowerThresholdCritical")})
				}
				for _, pc := range pw.List("PowerControl") {
					if w := fptr(pc, "PowerConsumedWatts"); w != nil {
						out = append(out, Sensor{Name: first(pc, "Name", "MemberId"), Kind: "power", Reading: w, Unit: "W", Health: NormHealth(pc.Str("Status.Health"))})
					}
				}
			}
		}
		if !got {
			if ref := ch.Ref("Sensors"); ref != "" {
				ms, err := r.c.Members(ctx, ref)
				if err == nil {
					for _, m := range ms {
						if absent(m) {
							continue
						}
						out = append(out, Sensor{Name: first(m, "Name", "Id"), Kind: sensorKind(m.Str("ReadingType")), Reading: fptr(m, "Reading"),
							Unit: unit(m.Str("ReadingUnits")), Health: NormHealth(m.Str("Status.Health")), State: m.Str("Status.State"),
							UpperCrit: fptr(m, "Thresholds.UpperCritical.Reading"), LowerCrit: fptr(m, "Thresholds.LowerCritical.Reading"), Location: m.Str("PhysicalContext")})
					}
				}
			}
		}
	}
	for i := range out {
		s := &out[i]
		if s.Health == "" && s.Reading != nil {
			if s.UpperCrit != nil && *s.Reading >= *s.UpperCrit {
				s.Health = Critical
			} else if s.LowerCrit != nil && *s.Reading <= *s.LowerCrit && s.Kind != "temperature" {
				s.Health = Critical
			}
		}
	}
	return out, nil
}

func sensorKind(t string) string {
	switch strings.ToLower(t) {
	case "temperature":
		return "temperature"
	case "rotational", "percent":
		return "fan"
	case "voltage":
		return "voltage"
	case "power", "energykwh":
		return "power"
	case "current":
		return "current"
	}
	if t == "" {
		return "other"
	}
	return strings.ToLower(t)
}

type logSvc struct {
	id, name, entries, typ, owner string
}

func (r *Redfish) logServices(ctx context.Context) []logSvc {
	var out []logSvc
	for _, owner := range []redfish.Object{r.system, r.manager} {
		ref := owner.Ref("LogServices")
		if ref == "" {
			continue
		}
		ms, err := r.c.Members(ctx, ref)
		if err != nil {
			continue
		}
		kind := "system"
		if owner.ID() == r.manager.ID() {
			kind = "bmc"
		}
		for _, m := range ms {
			out = append(out, logSvc{id: m.Str("Id"), name: m.Str("Name"), entries: m.Ref("Entries"), typ: m.Str("LogEntryType"), owner: kind})
		}
	}
	return out
}

// Events implements Backend.
func (r *Redfish) Events(ctx context.Context, limit int, allLogs bool) ([]Event, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	svcs := r.logServices(ctx)
	if len(svcs) == 0 {
		return nil, fmt.Errorf("%w: no LogServices exposed", ErrUnsupported)
	}
	var chosen []logSvc
	if allLogs {
		chosen = svcs
	} else {
		for _, want := range r.prof.Logs {
			for _, s := range svcs {
				if strings.EqualFold(s.id, want) {
					chosen = append(chosen, s)
				}
			}
			if len(chosen) > 0 {
				break
			}
		}
		if len(chosen) == 0 {
			for _, s := range svcs {
				if strings.EqualFold(s.typ, "SEL") || strings.Contains(strings.ToLower(s.id), "sel") {
					chosen = append(chosen, s)
				}
			}
		}
		if len(chosen) == 0 {
			chosen = svcs[:1]
		}
	}
	var out []Event
	for _, s := range chosen {
		if s.entries == "" {
			continue
		}
		evs, err := r.entries(ctx, s, limit)
		if err != nil {
			if errors.Is(err, redfish.ErrUnreachable) {
				return nil, err
			}
			continue
		}
		out = append(out, evs...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *Redfish) entries(ctx context.Context, s logSvc, limit int) ([]Event, error) {
	var raw []redfish.Object
	next := s.entries
	for pages := 0; next != "" && pages < 20 && len(raw) < 2000; pages++ {
		col, err := r.c.Get(ctx, next)
		if err != nil {
			return nil, err
		}
		for _, m := range col.List("Members") {
			if m.Str("Message") == "" && m.Str("MessageId") == "" && m.Str("Created") == "" {
				if full, err := r.c.Get(ctx, m.ID()); err == nil {
					m = full
				}
			}
			raw = append(raw, m)
		}
		next = col.Str("Members@odata.nextLink")
	}
	src := s.id
	if s.name != "" && !strings.EqualFold(s.name, s.id) {
		src = s.id + " (" + s.name + ")"
	}
	out := make([]Event, 0, len(raw))
	for _, m := range raw {
		sev := m.Str("Severity")
		if r.prof.SeverityKey != "" {
			if v := m.Str(r.prof.SeverityKey); v != "" {
				sev = v
			}
		}
		msg := m.Str("Message")
		if msg == "" {
			msg = m.Str("MessageId")
			if args := m.Strings("MessageArgs"); len(args) > 0 {
				msg += " " + strings.Join(args, ", ")
			}
		}
		out = append(out, Event{ID: m.Str("Id"), Time: normTime(m.Str("Created")), Severity: eventSeverity(sev), Message: strings.TrimSpace(msg), Source: src, Code: m.Str("MessageId")})
	}
	return out, nil
}

func normTime(s string) string {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

func eventSeverity(s string) string {
	switch strings.ToLower(s) {
	case "ok", "informational", "info", "repaired", "":
		return OK
	case "warning", "caution", "minor", "major":
		return Warning
	case "critical", "fatal":
		return Critical
	}
	return NormHealth(s)
}

// PowerState implements Backend.
func (r *Redfish) PowerState(ctx context.Context) (string, error) {
	if err := r.ready(ctx); err != nil {
		return "", err
	}
	// Re-read: power state is the one field callers poll.
	s, err := r.c.Get(ctx, r.system.ID())
	if err != nil {
		return "", err
	}
	return s.Str("PowerState"), nil
}

// Firmware implements Backend.
func (r *Redfish) Firmware(ctx context.Context) ([]Firmware, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	var out []Firmware
	refs := []string{}
	if us := r.root.Ref("UpdateService"); us != "" {
		if u, err := r.c.Get(ctx, us); err == nil && u.Ref("FirmwareInventory") != "" {
			refs = append(refs, u.Ref("FirmwareInventory"))
		}
	}
	if ref := r.system.Ref("Oem.Hp.Links.FirmwareInventory"); ref != "" { // iLO 4
		refs = append(refs, ref)
	}
	for _, ref := range refs {
		ms, err := r.c.Members(ctx, ref)
		if err != nil {
			continue
		}
		for _, m := range ms {
			id := m.Str("Id")
			if r.prof.HideFirmware != nil && r.prof.HideFirmware(id) {
				continue
			}
			upd, _ := m.Get("Updateable").(bool)
			out = append(out, Firmware{Name: first(m, "Name", "Id"), Version: m.Str("Version"), ID: id, Updateable: upd})
		}
		if len(out) > 0 {
			break
		}
	}
	if len(out) == 0 {
		if v := r.system.Str("BiosVersion"); v != "" {
			out = append(out, Firmware{Name: "BIOS", Version: v})
		}
		if v := r.manager.Str("FirmwareVersion"); v != "" {
			out = append(out, Firmware{Name: r.bmcLabel(), Version: v})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Boot implements Backend.
func (r *Redfish) Boot(ctx context.Context) (*Boot, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	s, err := r.c.Get(ctx, r.system.ID())
	if err != nil {
		return nil, err
	}
	b := &Boot{Order: []string{}, OverrideTarget: s.Str("Boot.BootSourceOverrideTarget"), OverrideMode: s.Str("Boot.BootSourceOverrideEnabled"),
		BootMode: s.Str("Boot.BootSourceOverrideMode"), AllowedTargets: s.Strings("Boot.BootSourceOverrideTarget@Redfish.AllowableValues")}
	names := map[string]string{}
	if ref := s.Ref("Boot.BootOptions"); ref != "" {
		if ms, err := r.c.Members(ctx, ref); err == nil {
			for _, m := range ms {
				name := first(m, "DisplayName", "Name")
				names[m.Str("BootOptionReference")] = name
				names[m.Str("Id")] = name
			}
		}
	}
	for _, o := range s.Strings("Boot.BootOrder") {
		if n, ok := names[o]; ok && n != "" {
			o = n + " (" + o + ")"
		}
		b.Order = append(b.Order, o)
	}
	return b, nil
}

// --- dangerous operations: plans only; execution happens after confirmation.

func (r *Redfish) action(o redfish.Object, name string) (target string, allowed []string) {
	a := o.Obj("Actions").Obj("#" + name)
	target = a.Str("target")
	allowed = a.Strings("ResetType@Redfish.AllowableValues")
	return
}

func (r *Redfish) actionInfo(ctx context.Context, o redfish.Object, name, param string) []string {
	a := o.Obj("Actions").Obj("#" + name)
	ref := a.Str("@Redfish.ActionInfo")
	if ref == "" {
		return nil
	}
	info, err := r.c.Get(ctx, ref)
	if err != nil {
		return nil
	}
	for _, p := range info.List("Parameters") {
		if p.Str("Name") == param {
			return p.Strings("AllowableValues")
		}
	}
	return nil
}

func has(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

var powerMap = map[string][]string{
	"on":            {"On", "ForceOn", "PushPowerButton"},
	"off":           {"GracefulShutdown"},
	"force-off":     {"ForceOff"},
	"restart":       {"GracefulRestart"},
	"force-restart": {"ForceRestart"},
	"cycle":         {"PowerCycle"},
	"nmi":           {"Nmi"},
}

func (r *Redfish) post(path string, body map[string]any) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		resp, err := r.c.Post(ctx, path, body)
		if err != nil {
			return "", err
		}
		if resp.Location != "" {
			return fmt.Sprintf("accepted (HTTP %d), task %s", resp.Status, resp.Location), nil
		}
		return fmt.Sprintf("accepted (HTTP %d)", resp.Status), nil
	}
}

// PlanPower implements Backend.
func (r *Redfish) PlanPower(ctx context.Context, action string) (*Plan, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	cands, ok := powerMap[action]
	if !ok {
		return nil, fmt.Errorf("unknown power action %q (want one of %s)", action, strings.Join(PowerActions, ", "))
	}
	s, err := r.c.Get(ctx, r.system.ID())
	if err != nil {
		return nil, err
	}
	target, allowed := r.action(s, "ComputerSystem.Reset")
	if target == "" {
		return nil, fmt.Errorf("%w: system has no ComputerSystem.Reset action", ErrUnsupported)
	}
	if len(allowed) == 0 {
		allowed = r.actionInfo(ctx, s, "ComputerSystem.Reset", "ResetType")
	}
	rt := ""
	for _, c := range cands {
		if len(allowed) == 0 || has(allowed, c) {
			rt = c
			break
		}
	}
	if rt == "" {
		return nil, fmt.Errorf("%w: %s offers ResetType %s, none means %q", ErrUnsupported, r.bmcLabel(), strings.Join(allowed, "/"), action)
	}
	body := map[string]any{"ResetType": rt}
	cur := s.Str("PowerState")
	sum := fmt.Sprintf("power %s %s (%s %s, ResetType=%s, currently %s)", action, r.target, r.prof.Vendor, r.bmcLabel(), rt, cur)
	return NewPlan(sum, "POST", target, body, cur, r.post(target, body)), nil
}

// PlanBMCReset implements Backend.
func (r *Redfish) PlanBMCReset(ctx context.Context) (*Plan, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	target, allowed := r.action(r.manager, "Manager.Reset")
	if target == "" {
		return nil, fmt.Errorf("%w: manager has no Manager.Reset action", ErrUnsupported)
	}
	body := map[string]any{}
	if !r.prof.BMCResetNoBody {
		switch {
		case len(allowed) == 0 || has(allowed, "GracefulRestart"):
			body["ResetType"] = "GracefulRestart"
		case has(allowed, "ForceRestart"):
			body["ResetType"] = "ForceRestart"
		}
	}
	sum := fmt.Sprintf("restart the BMC of %s (%s %s); the BMC is unreachable for a few minutes, the host keeps running", r.target, r.prof.Vendor, r.bmcLabel())
	return NewPlan(sum, "POST", target, body, "", r.post(target, body)), nil
}

var bootTargets = []string{"None", "Pxe", "Cd", "Usb", "Hdd", "BiosSetup", "Utilities", "Diags", "UefiShell", "UefiHttp", "UefiTarget", "SDCard", "Floppy", "RemoteDrive"}

// PlanBootOverride implements Backend.
func (r *Redfish) PlanBootOverride(ctx context.Context, target string, persistent bool) (*Plan, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	s, err := r.c.Get(ctx, r.system.ID())
	if err != nil {
		return nil, err
	}
	allowed := s.Strings("Boot.BootSourceOverrideTarget@Redfish.AllowableValues")
	canon := ""
	alias := map[string]string{"bios": "BiosSetup", "setup": "BiosSetup", "disk": "Hdd", "network": "Pxe", "dvd": "Cd", "http": "UefiHttp", "shell": "UefiShell"}
	if a, ok := alias[strings.ToLower(target)]; ok {
		target = a
	}
	for _, t := range bootTargets {
		if strings.EqualFold(t, target) {
			canon = t
		}
	}
	if canon == "" {
		return nil, fmt.Errorf("unknown boot target %q (want one of %s)", target, strings.Join(bootTargets, ", "))
	}
	if len(allowed) > 0 && !has(allowed, canon) {
		return nil, fmt.Errorf("%w: %s allows boot targets %s", ErrUnsupported, r.bmcLabel(), strings.Join(allowed, ", "))
	}
	enabled := "Once"
	if persistent {
		enabled = "Continuous"
	}
	if canon == "None" {
		enabled = "Disabled"
	}
	body := map[string]any{"Boot": map[string]any{"BootSourceOverrideTarget": canon, "BootSourceOverrideEnabled": enabled}}
	cur := s.Str("Boot.BootSourceOverrideTarget") + "/" + s.Str("Boot.BootSourceOverrideEnabled")
	path := r.system.ID()
	sum := fmt.Sprintf("set boot override on %s to %s (%s); takes effect at next boot, currently %s", r.target, canon, enabled, cur)
	return NewPlan(sum, "PATCH", path, body, cur, func(ctx context.Context) (string, error) {
		resp, err := r.c.Patch(ctx, path, body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("applied (HTTP %d)", resp.Status), nil
	}), nil
}

// PlanFirmwareUpdate implements Backend.
func (r *Redfish) PlanFirmwareUpdate(ctx context.Context, imageURI string) (*Plan, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	u, err := url.Parse(imageURI)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("image must be a URL the BMC can fetch (http/https/nfs/cifs), got %q", imageURI)
	}
	us := r.root.Ref("UpdateService")
	if us == "" {
		return nil, fmt.Errorf("%w: no UpdateService", ErrUnsupported)
	}
	svc, err := r.c.Get(ctx, us)
	if err != nil {
		return nil, err
	}
	target, _ := r.action(svc, "UpdateService.SimpleUpdate")
	if target == "" {
		return nil, fmt.Errorf("%w: UpdateService has no SimpleUpdate action", ErrUnsupported)
	}
	body := map[string]any{"ImageURI": imageURI}
	sum := fmt.Sprintf("flash firmware on %s (%s %s) from %s; the BMC applies it per its own policy (some components need a reboot)", r.target, r.prof.Vendor, r.bmcLabel(), imageURI)
	return NewPlan(sum, "POST", target, body, "", r.post(target, body)), nil
}

// oemAction finds an OEM action target either where iLO puts it
// (Oem.<Vendor>.Actions) or where DSP0266 puts it (Actions.Oem).
func oemAction(o redfish.Object, vendor, name string) string {
	if t := o.Obj("Oem").Obj(vendor).Obj("Actions").Obj(name).Str("target"); t != "" {
		return t
	}
	return o.Obj("Actions").Obj("Oem").Obj(name).Str("target")
}

// PlanVirtualMedia implements Backend.
func (r *Redfish) PlanVirtualMedia(ctx context.Context, image string, eject bool) (*Plan, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	var vm redfish.Object
	for _, owner := range []redfish.Object{r.manager, r.system} {
		ref := owner.Ref("VirtualMedia")
		if ref == "" {
			continue
		}
		ms, err := r.c.Members(ctx, ref)
		if err != nil {
			continue
		}
		for _, m := range ms {
			types := m.Strings("MediaTypes")
			if has(types, "CD") || has(types, "DVD") {
				vm = m
				break
			}
		}
		if vm != nil {
			break
		}
	}
	if vm == nil {
		return nil, fmt.Errorf("%w: no CD/DVD virtual media slot", ErrUnsupported)
	}
	id := vm.ID()
	cur := vm.Str("Image")
	if eject {
		if t := vm.Obj("Actions").Obj("#VirtualMedia.EjectMedia").Str("target"); t != "" {
			return NewPlan(fmt.Sprintf("eject virtual media %s on %s (currently %q)", vm.Str("Id"), r.target, cur), "POST", t, map[string]any{}, cur, r.post(t, map[string]any{})), nil
		}
		if t := oemAction(vm, "Hpe", "#HpeiLOVirtualMedia.EjectVirtualMedia"); t != "" {
			return NewPlan(fmt.Sprintf("eject virtual media on %s (iLO OEM action)", r.target), "POST", t, map[string]any{}, cur, r.post(t, map[string]any{})), nil
		}
		body := map[string]any{"Image": nil, "Inserted": false}
		return NewPlan(fmt.Sprintf("eject virtual media %s on %s (PATCH)", vm.Str("Id"), r.target), "PATCH", id, body, cur, func(ctx context.Context) (string, error) {
			resp, err := r.c.Patch(ctx, id, body)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("applied (HTTP %d)", resp.Status), nil
		}), nil
	}
	u, err := url.Parse(image)
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("image must be a URL the BMC can fetch, got %q", image)
	}
	sum := fmt.Sprintf("insert %s as virtual CD on %s (%s slot %s, currently %q)", image, r.target, r.bmcLabel(), vm.Str("Id"), cur)
	if t := vm.Obj("Actions").Obj("#VirtualMedia.InsertMedia").Str("target"); t != "" {
		body := map[string]any{"Image": image, "Inserted": true, "WriteProtected": true}
		return NewPlan(sum, "POST", t, body, cur, r.post(t, body)), nil
	}
	if t := oemAction(vm, "Hpe", "#HpeiLOVirtualMedia.InsertVirtualMedia"); t != "" {
		body := map[string]any{"Image": image}
		return NewPlan(sum+" via iLO OEM action", "POST", t, body, cur, r.post(t, body)), nil
	}
	if r.prof.Vendor == "Supermicro" {
		return nil, fmt.Errorf("%w: this Supermicro BMC only offers the OEM VirtualMediaConfig flow (X11); X12+ firmware exposes VirtualMedia.InsertMedia", ErrUnsupported)
	}
	body := map[string]any{"Image": image, "Inserted": true}
	return NewPlan(sum+" via PATCH", "PATCH", id, body, cur, func(ctx context.Context) (string, error) {
		resp, err := r.c.Patch(ctx, id, body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("applied (HTTP %d)", resp.Status), nil
	}), nil
}
