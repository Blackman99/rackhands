package bmc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Blackman99/rackhands/internal/redfish"
)

// LowLifeLeftPercent is where a drive's predicted media life left turns a
// healthy drive into a Warning. Most BMCs only flag wear once the drive itself
// does; ten percent leaves time to order a spare.
const LowLifeLeftPercent = 10

// StorageReport is the storage subsystem of one box: controllers, physical
// drives and logical volumes, read-only.
type StorageReport struct {
	Target string `json:"target"`
	Via    string `json:"via"`
	Vendor string `json:"vendor"`
	// Health is the worst component health; "" when the BMC exposes none.
	Health string `json:"health"`
	// Source is the Redfish resource read: "Storage" (DMTF), "SmartStorage"
	// (HPE iLO OEM, Smart Array), "SimpleStorage" (DMTF, no RAID), or "".
	Source      string              `json:"source"`
	Controllers []StorageController `json:"controllers"`
	Drives      []Drive             `json:"drives"`
	Volumes     []Volume            `json:"volumes"`
	Notes       []string            `json:"notes,omitempty"`
}

// StorageController is one RAID / HBA / SATA controller.
type StorageController struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Storage  string `json:"storage"` // the Storage resource (subsystem) it belongs to
	Model    string `json:"model,omitempty"`
	Serial   string `json:"serial,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	Health   string `json:"health,omitempty"`
	State    string `json:"state,omitempty"`
}

// Drive is one physical disk. Health is the BMC's own verdict, raised to
// Warning when failure is predicted or the media is nearly worn out.
type Drive struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Storage          string   `json:"storage"`
	Model            string   `json:"model,omitempty"`
	Serial           string   `json:"serial,omitempty"`
	MediaType        string   `json:"media_type,omitempty"` // HDD, SSD, SMR
	Protocol         string   `json:"protocol,omitempty"`   // SAS, SATA, NVMe
	CapacityBytes    int64    `json:"capacity_bytes,omitempty"`
	Location         string   `json:"location,omitempty"`
	Health           string   `json:"health,omitempty"`
	BMCHealth        string   `json:"bmc_health,omitempty"`
	State            string   `json:"state,omitempty"`
	RAIDStatus       string   `json:"raid_status,omitempty"` // controller's word: Online, Failed, Rebuilding, …
	LifeLeftPercent  *float64 `json:"life_left_percent,omitempty"`
	FailurePredicted *bool    `json:"failure_predicted,omitempty"`
	Operation        string   `json:"operation,omitempty"` // e.g. "Rebuild 37%"
}

// Volume is one logical drive / virtual disk.
type Volume struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Storage       string `json:"storage"`
	RAIDType      string `json:"raid_type,omitempty"`
	CapacityBytes int64  `json:"capacity_bytes,omitempty"`
	Drives        int    `json:"drives,omitempty"`
	Health        string `json:"health,omitempty"`
	State         string `json:"state,omitempty"`
	RAIDStatus    string `json:"raid_status,omitempty"`
	Operation     string `json:"operation,omitempty"`
}

// Problems lists every storage component that is not OK, worst first, in
// the same shape health uses.
func (s *StorageReport) Problems() []Problem {
	if s == nil {
		return nil
	}
	var out []Problem
	for _, c := range s.Controllers {
		if bad(c.Health) {
			detail := c.Model
			if strings.Contains(c.Name, c.Model) {
				detail = ""
			}
			if c.State != "" && c.State != "Enabled" {
				detail = joinNonEmpty(", ", detail, strings.ToLower(c.State))
			}
			out = append(out, Problem{Component: "storage controller " + c.Name, Health: c.Health, Detail: joinNonEmpty(" · ", detail, prefix("firmware ", c.Firmware))})
		}
	}
	for _, d := range s.Drives {
		if bad(d.Health) {
			out = append(out, Problem{Component: "drive " + d.Name, Health: d.Health, Detail: d.Why()})
		}
	}
	for _, v := range s.Volumes {
		if bad(v.Health) {
			out = append(out, Problem{Component: "volume " + v.Name, Health: v.Health, Detail: v.Why()})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return Rank(out[i].Health) > Rank(out[j].Health) })
	return out
}

func bad(h string) bool { return h != "" && h != OK }

// words turns a vendor enum into words: "InterimRecoveryMode" -> "interim recovery mode".
func words(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if i > 0 && r >= 'A' && r <= 'Z' && rs[i-1] >= 'a' && rs[i-1] <= 'z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// Why explains a drive's health in a few words, most useful first: what is
// wrong, then where it is and what to order.
func (d Drive) Why() string {
	var why []string
	if d.RAIDStatus != "" && !strings.EqualFold(d.RAIDStatus, "online") && !strings.EqualFold(d.RAIDStatus, "ok") {
		why = append(why, words(d.RAIDStatus))
	} else if d.BMCHealth != "" && d.BMCHealth != OK {
		why = append(why, map[string]string{Warning: "degraded", Critical: "failed"}[d.BMCHealth])
	}
	if d.FailurePredicted != nil && *d.FailurePredicted {
		why = append(why, "failure predicted")
	}
	if d.LifeLeftPercent != nil && *d.LifeLeftPercent < LowLifeLeftPercent {
		why = append(why, fmt.Sprintf("%g%% media life left", *d.LifeLeftPercent))
	}
	if d.Operation != "" {
		why = append(why, strings.ToLower(d.Operation))
	}
	what := joinNonEmpty(" ", d.MediaType, HumanBytes(d.CapacityBytes), d.Model)
	loc := d.Location
	if strings.EqualFold(loc, d.Name) {
		loc = ""
	}
	return joinNonEmpty(" · ", strings.Join(why, ", "), what, loc, prefix("S/N ", d.Serial), prefix("on ", d.Storage))
}

// Why explains a volume's health.
func (v Volume) Why() string {
	state := ""
	if v.RAIDStatus != "" && !strings.EqualFold(v.RAIDStatus, "online") && !strings.EqualFold(v.RAIDStatus, "ok") {
		state = words(v.RAIDStatus)
	} else if v.Health != OK {
		state = map[string]string{Warning: "degraded", Critical: "failed"}[v.Health]
	}
	return joinNonEmpty(" · ", joinNonEmpty(", ", state, strings.ToLower(v.Operation)), joinNonEmpty(" ", v.RAIDType, HumanBytes(v.CapacityBytes)),
		prefix("on ", v.Storage))
}

// Summary is the one-line storage roll call health prints, e.g.
// "1 controller · 5 drives (1 Critical) · 2 volumes (1 Warning)".
func (s *StorageReport) Summary() string {
	if s == nil {
		return ""
	}
	if s.Source == "" {
		return "not exposed by this BMC"
	}
	count := func(n int, one string, hs []string) string {
		out := plural(n, one)
		byLevel := map[string]int{}
		for _, h := range hs {
			if bad(h) {
				byLevel[h]++
			}
		}
		var parts []string
		for _, l := range []string{Critical, Warning, Unknown} {
			if byLevel[l] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", byLevel[l], l))
			}
		}
		if len(parts) > 0 {
			out += " (" + strings.Join(parts, ", ") + ")"
		}
		return out
	}
	var ch, dh, vh []string
	for _, c := range s.Controllers {
		ch = append(ch, c.Health)
	}
	for _, d := range s.Drives {
		dh = append(dh, d.Health)
	}
	for _, v := range s.Volumes {
		vh = append(vh, v.Health)
	}
	parts := []string{count(len(s.Controllers), "controller", ch), count(len(s.Drives), "drive", dh)}
	if s.Source != "SimpleStorage" || len(s.Volumes) > 0 {
		parts = append(parts, count(len(s.Volumes), "volume", vh))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func prefix(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

func joinNonEmpty(sep string, xs ...string) string {
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return strings.Join(out, sep)
}

// HumanBytes prints a capacity the way drives are sold (decimal): 960 GB, 2.4 TB.
func HumanBytes(b int64) string {
	if b <= 0 {
		return ""
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f, i := float64(b), 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if f >= 100 || f == math.Trunc(f) {
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	if f >= 10 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0") + " " + units[i]
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", f), "0"), ".") + " " + units[i]
}

// raidFromVolumeType maps the pre-2019 VolumeType enum (all some BMCs
// serve) onto a RAID level.
func raidFromVolumeType(t string) string {
	switch t {
	case "RawDevice", "NonRedundant":
		return "RAID0"
	case "Mirrored":
		return "RAID1"
	case "StripedWithParity":
		return "RAID5/6"
	case "SpannedMirrors":
		return "RAID10"
	case "SpannedStripesWithParity":
		return "RAID50/60"
	}
	return t
}

// Storage implements Backend: DMTF Storage first, then HPE's SmartStorage
// (iLO 4, and iLO 5 with Smart Array controllers), then SimpleStorage.
func (r *Redfish) Storage(ctx context.Context) (*StorageReport, error) {
	if err := r.ready(ctx); err != nil {
		return nil, err
	}
	rep := &StorageReport{Target: r.target, Via: "redfish", Vendor: r.prof.Vendor,
		Controllers: []StorageController{}, Drives: []Drive{}, Volumes: []Volume{}}
	empty := func() bool { return len(rep.Controllers)+len(rep.Drives)+len(rep.Volumes) == 0 }
	try := func(source, ref string, read func(context.Context, string, *StorageReport) error) error {
		if ref == "" || !empty() {
			return nil
		}
		if err := read(ctx, ref, rep); err != nil {
			if errors.Is(err, redfish.ErrUnreachable) {
				return err
			}
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s not readable: %s", source, shortErr(err)))
			return nil
		}
		if !empty() {
			rep.Source = source
		}
		return nil
	}
	if err := try("Storage", r.system.Ref("Storage"), r.dmtfStorage); err != nil {
		return nil, err
	}
	if r.prof.Vendor == "HPE" {
		ref := first(r.system, "Oem.Hpe.Links.SmartStorage.@odata.id", "Oem.Hp.Links.SmartStorage.@odata.id")
		if err := try("SmartStorage", ref, r.smartStorage); err != nil {
			return nil, err
		}
		if rep.Source == "SmartStorage" {
			rep.Notes = append(rep.Notes, "Smart Array read from the iLO OEM SmartStorage tree (no DMTF Storage for it on this iLO)")
		}
	}
	if err := try("SimpleStorage", r.system.Ref("SimpleStorage"), r.simpleStorage); err != nil {
		return nil, err
	}
	switch rep.Source {
	case "":
		rep.Notes = append(rep.Notes, "this BMC exposes no storage inventory (no Storage, SmartStorage or SimpleStorage with devices)")
	case "SimpleStorage":
		rep.Notes = append(rep.Notes, "only SimpleStorage is exposed: drive health, but no RAID volumes, wear or failure prediction")
	}
	r.qualifyDuplicateNames(rep)
	var hs []string
	for _, c := range rep.Controllers {
		hs = append(hs, c.Health)
	}
	for _, d := range rep.Drives {
		hs = append(hs, d.Health)
	}
	for _, v := range rep.Volumes {
		hs = append(hs, v.Health)
	}
	for _, h := range hs {
		if h != "" {
			rep.Health = Worst(hs...)
			break
		}
	}
	return rep, nil
}

// qualifyDuplicateNames appends the subsystem to names that repeat across
// controllers ("Drive 0" on two adapters), so problems stay distinguishable.
func (r *Redfish) qualifyDuplicateNames(rep *StorageReport) {
	seen := map[string]int{}
	for _, d := range rep.Drives {
		seen[d.Name]++
	}
	for i, d := range rep.Drives {
		if seen[d.Name] > 1 && d.Storage != "" {
			rep.Drives[i].Name = d.Name + " (" + d.Storage + ")"
		}
	}
	seen = map[string]int{}
	for _, v := range rep.Volumes {
		seen[v.Name]++
	}
	for i, v := range rep.Volumes {
		if seen[v.Name] > 1 && v.Storage != "" {
			rep.Volumes[i].Name = v.Name + " (" + v.Storage + ")"
		}
	}
}

// getAll fetches refs with bounded concurrency, keeping order. Broken
// members are skipped; an unreachable BMC aborts.
func (r *Redfish) getAll(ctx context.Context, refs []string) ([]redfish.Object, error) {
	out := make([]redfish.Object, len(refs))
	errs := make([]error, len(refs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i], errs[i] = r.c.Get(ctx, ref)
		}(i, ref)
	}
	wg.Wait()
	res := out[:0]
	for i, o := range out {
		if errs[i] != nil {
			if errors.Is(errs[i], redfish.ErrUnreachable) {
				return nil, errs[i]
			}
			continue
		}
		res = append(res, o)
	}
	return res, nil
}

func absent(o redfish.Object) bool { return strings.EqualFold(o.Str("Status.State"), "Absent") }

func boolPtr(o redfish.Object, k string) *bool {
	if b, ok := o.Get(k).(bool); ok {
		return &b
	}
	return nil
}

func int64Of(o redfish.Object, k string, mul float64) int64 {
	if v, ok := o.Num(k); ok && v > 0 {
		return int64(v * mul)
	}
	return 0
}

func operation(o redfish.Object) string {
	for _, op := range o.List("Operations") {
		name := op.Str("OperationName")
		if name == "" {
			continue
		}
		if p, ok := op.Num("PercentageComplete"); ok {
			return fmt.Sprintf("%s %g%%", name, p)
		}
		return name
	}
	return ""
}

func nameOr(o redfish.Object, fallback string) string {
	if n := o.Str("Name"); n != "" {
		return n
	}
	if id := o.Str("Id"); id != "" {
		return id
	}
	return fallback
}

// driveHealth raises the BMC's verdict when the drive warns ahead of time.
func driveHealth(d *Drive) {
	d.Health = d.BMCHealth
	if Rank(d.Health) >= Rank(Warning) {
		return
	}
	if (d.FailurePredicted != nil && *d.FailurePredicted) || (d.LifeLeftPercent != nil && *d.LifeLeftPercent < LowLifeLeftPercent) {
		d.Health = Warning
	}
}

func driveLocation(o redfish.Object) string {
	pl := o.Obj("PhysicalLocation.PartLocation")
	if l := pl.Str("ServiceLabel"); l != "" {
		return l
	}
	if n, ok := pl.Num("LocationOrdinalValue"); ok {
		t := pl.Str("LocationType")
		if t == "" || t == "Slot" {
			t = "Slot"
		}
		return fmt.Sprintf("%s %g", t, n)
	}
	if l := o.List("Location"); len(l) > 0 {
		return l[0].Str("Info")
	}
	return ""
}

// dmtfStorage reads the Storage collection (DSP0268 Storage, Drive, Volume).
func (r *Redfish) dmtfStorage(ctx context.Context, ref string, rep *StorageReport) error {
	subs, err := r.c.Members(ctx, ref)
	if err != nil {
		return err
	}
	for _, st := range subs {
		sid := st.Str("Id")
		sname := nameOr(st, sid)
		var ctrls []redfish.Object
		if cref := st.Ref("Controllers"); cref != "" {
			if ctrls, err = r.c.Members(ctx, cref); err != nil && errors.Is(err, redfish.ErrUnreachable) {
				return err
			}
		}
		if len(ctrls) == 0 {
			ctrls = st.List("StorageControllers")
		}
		drives, err := r.getAll(ctx, st.Refs("Drives"))
		if err != nil {
			return err
		}
		var vols []redfish.Object
		if vref := st.Ref("Volumes"); vref != "" {
			if vols, err = r.c.Members(ctx, vref); err != nil && errors.Is(err, redfish.ErrUnreachable) {
				return err
			}
		}
		present := 0
		for _, d := range drives {
			if !absent(d) {
				present++
			}
		}
		// iDRAC lists every storage-capable device (chipset SATA, CPU-attached
		// NVMe) even with nothing behind it; leave healthy empty ones out.
		if present == 0 && len(vols) == 0 && !bad(NormHealth(st.Str("Status.Health"))) {
			continue
		}
		if len(ctrls) == 0 {
			ctrls = []redfish.Object{st}
		}
		for i, c := range ctrls {
			if absent(c) {
				continue
			}
			rep.Controllers = append(rep.Controllers, StorageController{
				ID: first(c, "Id", "MemberId"), Name: nameOr(c, sname), Storage: sid, Model: c.Str("Model"),
				Serial: c.Str("SerialNumber"), Firmware: c.Str("FirmwareVersion"),
				Health: NormHealth(c.Str("Status.Health")), State: c.Str("Status.State"),
			})
			if rep.Controllers[len(rep.Controllers)-1].ID == "" {
				rep.Controllers[len(rep.Controllers)-1].ID = strconv.Itoa(i)
			}
		}
		for _, o := range drives {
			if absent(o) {
				continue
			}
			d := Drive{ID: o.Str("Id"), Name: nameOr(o, o.ID()), Storage: sname, Model: o.Str("Model"), Serial: o.Str("SerialNumber"),
				MediaType: o.Str("MediaType"), Protocol: o.Str("Protocol"), CapacityBytes: int64Of(o, "CapacityBytes", 1),
				Location: driveLocation(o), BMCHealth: NormHealth(o.Str("Status.Health")), State: o.Str("Status.State"),
				RAIDStatus: o.Str("Oem.Dell.DellPhysicalDisk.RaidStatus"), LifeLeftPercent: fptr(o, "PredictedMediaLifeLeftPercent"),
				FailurePredicted: boolPtr(o, "FailurePredicted"), Operation: operation(o)}
			driveHealth(&d)
			rep.Drives = append(rep.Drives, d)
		}
		for _, o := range vols {
			if absent(o) {
				continue
			}
			raid := o.Str("RAIDType")
			if raid == "" {
				raid = raidFromVolumeType(o.Str("VolumeType"))
			}
			n := len(o.Refs("Links.Drives"))
			if c, ok := o.Num("Links.Drives@odata.count"); ok && n == 0 {
				n = int(c)
			}
			rep.Volumes = append(rep.Volumes, Volume{ID: o.Str("Id"), Name: nameOr(o, o.ID()), Storage: sname, RAIDType: raid,
				CapacityBytes: int64Of(o, "CapacityBytes", 1), Drives: n, Health: NormHealth(o.Str("Status.Health")),
				State: o.Str("Status.State"), RAIDStatus: o.Str("Oem.Dell.DellVolume.RaidStatus"), Operation: operation(o)})
		}
	}
	return nil
}

// statusReasons keeps the reasons iLO gives besides a plain "OK".
func statusReasons(o redfish.Object, k string) string {
	var out []string
	for _, s := range o.Strings(k) {
		if !strings.EqualFold(s, "ok") {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

// smartStorage reads HPE's OEM SmartStorage tree (iLO 4 HpSmartStorage*,
// iLO 5 HpeSmartStorage*): sizes in MiB, RAID level as "5" or "1+0", SSD
// wear as endurance *used*.
func (r *Redfish) smartStorage(ctx context.Context, ref string, rep *StorageReport) error {
	ss, err := r.c.Get(ctx, ref)
	if err != nil {
		return err
	}
	acRef := ss.Ref("Links.ArrayControllers")
	if acRef == "" {
		acRef = ss.Ref("ArrayControllers")
	}
	if acRef == "" {
		return nil
	}
	acs, err := r.c.Members(ctx, acRef)
	if err != nil {
		return err
	}
	const mib = 1 << 20
	for i, ac := range acs {
		if absent(ac) {
			continue
		}
		name := first(ac, "Model", "Name")
		if loc := ac.Str("Location"); loc != "" {
			name += " (" + loc + ")"
		}
		rep.Controllers = append(rep.Controllers, StorageController{ID: first(ac, "Id"), Name: name, Storage: "ArrayController " + strconv.Itoa(i),
			Model: ac.Str("Model"), Serial: ac.Str("SerialNumber"),
			Firmware: first(ac, "FirmwareVersion.Current.VersionString", "FirmwareVersion"),
			Health:   NormHealth(ac.Str("Status.Health")), State: ac.Str("Status.State")})
		if pref := first(ac, "Links.PhysicalDrives.@odata.id"); pref != "" {
			ds, err := r.c.Members(ctx, pref)
			if err != nil && errors.Is(err, redfish.ErrUnreachable) {
				return err
			}
			for _, o := range ds {
				if absent(o) {
					continue
				}
				d := Drive{ID: o.Str("Id"), Name: "Drive " + first(o, "Location", "Id"), Storage: name, Model: o.Str("Model"),
					Serial: o.Str("SerialNumber"), MediaType: o.Str("MediaType"), Protocol: o.Str("InterfaceType"),
					CapacityBytes: int64Of(o, "CapacityMiB", mib),
					BMCHealth:     NormHealth(o.Str("Status.Health")), State: o.Str("Status.State"),
					RAIDStatus: statusReasons(o, "DiskDriveStatusReasons")}
				if used, ok := o.Num("SSDEnduranceUtilizationPercentage"); ok {
					left := 100 - used
					d.LifeLeftPercent = &left
				}
				driveHealth(&d)
				rep.Drives = append(rep.Drives, d)
			}
		}
		if lref := first(ac, "Links.LogicalDrives.@odata.id"); lref != "" {
			ls, err := r.c.Members(ctx, lref)
			if err != nil && errors.Is(err, redfish.ErrUnreachable) {
				return err
			}
			for _, o := range ls {
				raid := o.Str("Raid")
				if raid != "" {
					raid = "RAID" + strings.ReplaceAll(raid, "+", "")
				}
				n := 0
				if dref := first(o, "Links.DataDrives.@odata.id"); dref != "" {
					if col, err := r.c.Get(ctx, dref); err == nil {
						n = len(col.Refs("Members"))
					}
				}
				vname := o.Str("LogicalDriveName")
				if vname == "" {
					vname = "Logical Drive " + o.Str("Id")
				}
				rep.Volumes = append(rep.Volumes, Volume{ID: o.Str("Id"), Name: vname, Storage: name, RAIDType: raid,
					CapacityBytes: int64Of(o, "CapacityMiB", mib), Drives: n, Health: NormHealth(o.Str("Status.Health")),
					State: o.Str("Status.State"), RAIDStatus: statusReasons(o, "LogicalDriveStatusReasons")})
			}
		}
	}
	return nil
}

// simpleStorage reads SimpleStorage: controllers with inline Devices only.
func (r *Redfish) simpleStorage(ctx context.Context, ref string, rep *StorageReport) error {
	ms, err := r.c.Members(ctx, ref)
	if err != nil {
		return err
	}
	for _, s := range ms {
		devs := s.List("Devices")
		present := 0
		for _, d := range devs {
			if !absent(d) {
				present++
			}
		}
		if absent(s) || (present == 0 && !bad(NormHealth(s.Str("Status.Health")))) {
			continue
		}
		name := nameOr(s, "")
		if desc := s.Str("Description"); desc != "" && desc != name {
			name += " (" + desc + ")"
		}
		rep.Controllers = append(rep.Controllers, StorageController{ID: s.Str("Id"), Name: name, Storage: s.Str("Id"),
			Health: NormHealth(s.Str("Status.Health")), State: s.Str("Status.State")})
		for _, o := range devs {
			if absent(o) {
				continue
			}
			d := Drive{ID: o.Str("Name"), Name: o.Str("Name"), Storage: name, Model: joinNonEmpty(" ", o.Str("Manufacturer"), o.Str("Model")),
				CapacityBytes: int64Of(o, "CapacityBytes", 1), BMCHealth: NormHealth(o.Str("Status.Health")), State: o.Str("Status.State")}
			driveHealth(&d)
			rep.Drives = append(rep.Drives, d)
		}
	}
	return nil
}
