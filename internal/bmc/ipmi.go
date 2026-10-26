package bmc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bougou/go-ipmi/pkg/client"
	"github.com/bougou/go-ipmi/pkg/command/chassis"
	"github.com/bougou/go-ipmi/pkg/types"
)

// IPMI is the IPMI 2.0 (RMCP+/lanplus) fallback backend, native Go, no
// ipmitool needed. It covers power, sensors, SEL, FRU, BMC reset and boot
// device; firmware inventory/update and virtual media are Redfish-only.
type IPMI struct {
	host, user, pass string
	port             int
	target           string
	lan15            bool
	timeout          time.Duration

	mu sync.Mutex
	c  *client.Client
}

// NewIPMI builds the backend; it connects lazily.
func NewIPMI(target, host string, port int, user, pass string, lan15 bool, timeout time.Duration) *IPMI {
	if port == 0 {
		port = 623
	}
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &IPMI{host: host, port: port, user: user, pass: pass, target: target, lan15: lan15, timeout: timeout}
}

func (b *IPMI) Protocol() string { return "ipmi" }

func (b *IPMI) conn(ctx context.Context) (*client.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.c != nil {
		return b.c, nil
	}
	var last error
	for _, lvl := range sessionLevels {
		c, err := b.dial(ctx, lvl)
		if err == nil {
			b.c = c
			return c, nil
		}
		last = err
		if !roleRejected(err) {
			break
		}
	}
	return nil, last
}

// sessionLevels are tried in order: a read-only (User) account is refused an
// Administrator session at RAKP time, so step down instead of failing.
var sessionLevels = []types.PrivilegeLevel{types.PrivilegeLevelAdministrator, types.PrivilegeLevelOperator, types.PrivilegeLevelUser}

// roleRejected: the BMC refused the requested role (not the password).
func roleRejected(err error) bool {
	var se *types.RmcpStatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.StatusCode() {
	case types.RmcpStatusCodeInvalidRole, types.RmcpStatusCodeUnauthorizedRoleOfPriLevel,
		types.RmcpStatusCodeNoResToCreateSessAtRole, types.RmcpStatusCodeUnauthorizedName:
		return true
	}
	return false
}

func (b *IPMI) dial(ctx context.Context, lvl types.PrivilegeLevel) (*client.Client, error) {
	c, err := client.NewClient(b.host, b.port, b.user, b.pass)
	if err != nil {
		return nil, err
	}
	c.WithMaxPrivilegeLevel(lvl)
	if b.lan15 {
		c.WithInterface(client.InterfaceLan)
	} else {
		c.WithInterface(client.InterfaceLanplus)
		// Try the two suites virtually every BMC supports instead of asking
		// with Get Channel Cipher Suites first: some BMCs (and some ipmi_sim
		// builds) never answer that command, which costs 5 x timeout.
		c.WithCipherSuiteID(types.CipherSuiteID17, types.CipherSuiteID3)
	}
	c.WithTimeout(b.timeout)
	if os.Getenv("RACKHANDS_IPMI_DEBUG") != "" {
		c.WithDebug(true)
	}
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("ipmi connect %s:%d: %w", b.host, b.port, err)
	}
	return c, nil
}

// Close ends the RMCP+ session.
func (b *IPMI) Close(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.c != nil {
		_ = b.c.Close(ctx)
		b.c = nil
	}
}

// IANA enterprise numbers of common BMC vendors.
var ianaVendor = map[uint32]string{674: "Dell", 11: "HPE", 47196: "HPE", 19046: "Lenovo", 2: "Lenovo", 10876: "Supermicro", 7244: "Quanta", 343: "Intel", 4128: "Cisco"}

func (b *IPMI) Vendor(ctx context.Context) string {
	c, err := b.conn(ctx)
	if err != nil {
		return ""
	}
	id, err := c.GetDeviceID(ctx)
	if err != nil {
		return ""
	}
	if v, ok := ianaVendor[id.ManufacturerID]; ok {
		return v
	}
	return "Generic"
}

func (b *IPMI) Inventory(ctx context.Context) (*Inventory, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return nil, err
	}
	inv := &Inventory{Target: b.target, Via: "ipmi", Vendor: b.Vendor(ctx), BMC: "IPMI BMC"}
	if id, err := c.GetDeviceID(ctx); err == nil {
		inv.BMCFirmware = fmt.Sprintf("%d.%02x", id.MajorFirmwareRevision, id.MinorFirmwareRevision)
		inv.Notes = append(inv.Notes, fmt.Sprintf("IPMI %d.%d, manufacturer id %d, product id %d", id.MajorIPMIVersion, id.MinorIPMIVersion, id.ManufacturerID, id.ProductID))
	}
	if st, err := c.GetChassisStatus(ctx); err == nil {
		inv.PowerState = powerWord(st.PowerIsOn)
	}
	if frus, err := c.GetFRUs(ctx); err == nil {
		for _, f := range frus {
			if f == nil || !f.Present() {
				continue
			}
			if p := f.ProductInfoArea; p != nil {
				setIf(&inv.Manufacturer, types.FRUFieldString(p.ManufacturerTypeLength, p.Manufacturer))
				setIf(&inv.Model, types.FRUFieldString(p.NameTypeLength, p.Name))
				setIf(&inv.Serial, types.FRUFieldString(p.SerialNumberTypeLength, p.SerialNumber))
			}
			if bd := f.BoardInfoArea; bd != nil {
				setIf(&inv.Manufacturer, types.FRUFieldString(bd.ManufacturerTypeLength, bd.Manufacturer))
				setIf(&inv.Model, types.FRUFieldString(bd.ProductNameTypeLength, bd.ProductName))
				setIf(&inv.Serial, types.FRUFieldString(bd.SerialNumberTypeLength, bd.SerialNumber))
			}
		}
	}
	if h, err := b.Health(ctx); err == nil {
		inv.Health = h.Overall
	}
	return inv, nil
}

func setIf(dst *string, v string) {
	v = strings.TrimSpace(v)
	if *dst == "" && v != "" {
		*dst = v
	}
}

func powerWord(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}

func thresholdHealth(st types.SensorThresholdStatus) string {
	switch st {
	case types.SensorThresholdStatus_OK:
		return OK
	case types.SensorThresholdStatus_LNC, types.SensorThresholdStatus_UNC:
		return Warning
	case types.SensorThresholdStatus_LCR, types.SensorThresholdStatus_UCR, types.SensorThresholdStatus_LNR, types.SensorThresholdStatus_UNR:
		return Critical
	}
	return ""
}

func ipmiKind(t string) string {
	l := strings.ToLower(t)
	switch {
	case strings.Contains(l, "temp"):
		return "temperature"
	case strings.Contains(l, "fan"):
		return "fan"
	case strings.Contains(l, "volt"):
		return "voltage"
	case strings.Contains(l, "power supply"):
		return "psu"
	case strings.Contains(l, "current"):
		return "current"
	}
	return "other"
}

func (b *IPMI) Sensors(ctx context.Context) ([]Sensor, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return nil, err
	}
	ss, err := retryReservation(ctx, func() ([]*types.Sensor, error) { return c.GetSensors(ctx) })
	if err != nil {
		return nil, err
	}
	out := make([]Sensor, 0, len(ss))
	for _, s := range ss {
		if s == nil || s.NotPresent {
			continue
		}
		o := Sensor{Name: strings.TrimSpace(s.Name), Kind: ipmiKind(fmt.Sprint(s.SensorType))}
		if s.IsThreshold() {
			if s.IsReadingValid() {
				v := s.Value
				o.Reading = &v
				o.Health = thresholdHealth(s.Threshold.ThresholdStatus)
			}
			o.Unit = unitShort(fmt.Sprint(s.SensorUnit))
		} else {
			o.State = s.HumanStr()
		}
		out = append(out, o)
	}
	return out, nil
}

func unitShort(u string) string {
	l := strings.ToLower(u)
	switch {
	case strings.Contains(l, "degrees c"):
		return "C"
	case strings.Contains(l, "rpm"):
		return "RPM"
	case strings.Contains(l, "volts"):
		return "V"
	case strings.Contains(l, "watts"):
		return "W"
	case strings.Contains(l, "amps"):
		return "A"
	}
	return strings.TrimSpace(u)
}

func (b *IPMI) Health(ctx context.Context) (*HealthReport, error) {
	ss, err := b.Sensors(ctx)
	if err != nil {
		return nil, err
	}
	rep := &HealthReport{Target: b.target, Via: "ipmi", Vendor: b.Vendor(ctx), Problems: []Problem{}, Overall: OK}
	levels := []string{OK}
	for _, s := range ss {
		if s.Health == Warning || s.Health == Critical {
			d := ""
			if s.Reading != nil {
				d = fmt.Sprintf("reading %g %s", *s.Reading, s.Unit)
			}
			rep.Problems = append(rep.Problems, Problem{Component: s.Kind + " " + s.Name, Health: s.Health, Detail: d})
			levels = append(levels, s.Health)
		}
	}
	if c, err := b.conn(ctx); err == nil {
		if st, err := c.GetChassisStatus(ctx); err == nil {
			if st.PowerFault || st.PowerOverload || st.PowerControlFault || st.InterLock {
				rep.Problems = append(rep.Problems, Problem{Component: "chassis power", Health: Critical, Detail: "chassis status reports a power fault"})
				levels = append(levels, Critical)
			}
			if st.CollingFanFault {
				rep.Problems = append(rep.Problems, Problem{Component: "chassis cooling", Health: Critical, Detail: "chassis status reports a fan fault"})
				levels = append(levels, Critical)
			}
			if st.DriveFault {
				rep.Problems = append(rep.Problems, Problem{Component: "chassis drives", Health: Critical, Detail: "chassis status reports a drive fault"})
				levels = append(levels, Critical)
			}
		}
	}
	rep.Overall = Worst(levels...)
	rep.Notes = append(rep.Notes, "IPMI health is derived from threshold sensors and chassis status",
		"storage not checked: IPMI has no controller, drive or RAID volume data (needs Redfish)")
	return rep, nil
}

func (b *IPMI) Events(ctx context.Context, limit int, _ bool) ([]Event, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return nil, err
	}
	sels, err := retryReservation(ctx, func() ([]*types.SEL, error) { return c.GetSELEntries(ctx, 0) })
	if err != nil {
		return nil, err
	}
	names := map[uint8]string{}
	if ss, err := c.GetSensors(ctx); err == nil {
		for _, s := range ss {
			names[s.Number] = strings.TrimSpace(s.Name)
		}
	}
	out := []Event{}
	for _, e := range sels {
		ev := Event{ID: fmt.Sprintf("%d", e.RecordID), Source: "SEL", Severity: OK}
		switch {
		case e.Standard != nil:
			s := e.Standard
			if s.Timestamp.Year() >= 2000 { // earlier = BMC clock not yet set ("pre-init")
				ev.Time = s.Timestamp.UTC().Format(time.RFC3339)
			}
			name := names[uint8(s.SensorNumber)]
			if name == "" {
				name = fmt.Sprintf("%v #%d", s.SensorType, s.SensorNumber)
			}
			dir := "asserted"
			if s.EventDir {
				dir = "deasserted"
			}
			ev.Message = strings.TrimSpace(fmt.Sprintf("%s: %s %s", name, s.EventString(), dir))
			ev.Severity = eventSeverity(string(s.EventSeverity()))
		case e.OEMTimestamped != nil:
			if e.OEMTimestamped.Timestamp.Year() >= 2000 {
				ev.Time = e.OEMTimestamped.Timestamp.UTC().Format(time.RFC3339)
			}
			ev.Message = "OEM timestamped record"
		default:
			ev.Message = "OEM record"
		}
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (b *IPMI) PowerState(ctx context.Context) (string, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return "", err
	}
	st, err := c.GetChassisStatus(ctx)
	if err != nil {
		return "", err
	}
	return powerWord(st.PowerIsOn), nil
}

func (b *IPMI) Firmware(ctx context.Context) ([]Firmware, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return nil, err
	}
	id, err := c.GetDeviceID(ctx)
	if err != nil {
		return nil, err
	}
	return []Firmware{{Name: "BMC", Version: fmt.Sprintf("%d.%02x", id.MajorFirmwareRevision, id.MinorFirmwareRevision)}}, nil
}

func (b *IPMI) Boot(ctx context.Context) (*Boot, error) {
	return nil, fmt.Errorf("%w: boot order is not readable over IPMI; use Redfish", ErrUnsupported)
}

var ipmiPower = map[string]chassis.ChassisControl{
	"on": chassis.ChassisControlPowerUp, "off": chassis.ChassisControlSoftShutdown, "force-off": chassis.ChassisControlPowerDown,
	"force-restart": chassis.ChassisControlHardReset, "cycle": chassis.ChassisControlPowerCycle, "nmi": chassis.ChassisControlDiagnosticInterrupt,
}

func (b *IPMI) PlanPower(ctx context.Context, action string) (*Plan, error) {
	ctl, ok := ipmiPower[action]
	if !ok {
		if _, known := powerMap[action]; known {
			return nil, fmt.Errorf("%w: IPMI has no %q (graceful restart needs Redfish)", ErrUnsupported, action)
		}
		return nil, fmt.Errorf("unknown power action %q", action)
	}
	cur, err := b.PowerState(ctx)
	if err != nil {
		return nil, err
	}
	sum := fmt.Sprintf("power %s %s over IPMI (chassis control 0x%02x, currently %s)", action, b.target, uint8(ctl), cur)
	return NewPlan(sum, "IPMI", "Chassis Control", map[string]any{"control": uint8(ctl)}, cur, func(ctx context.Context) (string, error) {
		c, err := b.conn(ctx)
		if err != nil {
			return "", err
		}
		if _, err := c.ChassisControl(ctx, ctl); err != nil {
			return "", err
		}
		return "chassis control accepted", nil
	}), nil
}

func (b *IPMI) PlanBMCReset(ctx context.Context) (*Plan, error) {
	if _, err := b.conn(ctx); err != nil {
		return nil, err
	}
	return NewPlan(fmt.Sprintf("cold-reset the BMC of %s over IPMI; the host keeps running", b.target), "IPMI", "Cold Reset", nil, "", func(ctx context.Context) (string, error) {
		c, err := b.conn(ctx)
		if err != nil {
			return "", err
		}
		if err := c.ColdReset(ctx); err != nil {
			return "", err
		}
		return "cold reset accepted", nil
	}), nil
}

var ipmiBoot = map[string]types.BootDeviceSelector{
	"none": types.BootDeviceSelectorNoOverride, "pxe": types.BootDeviceSelectorForcePXE, "hdd": types.BootDeviceSelectorForceHardDrive,
	"cd": types.BootDeviceSelectorForceCDROM, "biossetup": types.BootDeviceSelectorForceBIOSSetup, "diags": types.BootDeviceSelectorForceDiagnosticPartition,
	"bios": types.BootDeviceSelectorForceBIOSSetup, "disk": types.BootDeviceSelectorForceHardDrive, "network": types.BootDeviceSelectorForcePXE, "dvd": types.BootDeviceSelectorForceCDROM,
}

func (b *IPMI) PlanBootOverride(ctx context.Context, target string, persistent bool) (*Plan, error) {
	sel, ok := ipmiBoot[strings.ToLower(target)]
	if !ok {
		return nil, fmt.Errorf("%w: IPMI boot targets are none, pxe, hdd, cd, biossetup, diags", ErrUnsupported)
	}
	if _, err := b.conn(ctx); err != nil {
		return nil, err
	}
	mode := "next boot only"
	if persistent {
		mode = "persistent"
	}
	return NewPlan(fmt.Sprintf("set IPMI boot device of %s to %s (%s, UEFI)", b.target, strings.ToLower(target), mode), "IPMI", "Set System Boot Options", map[string]any{"device": uint8(sel)}, "", func(ctx context.Context) (string, error) {
		c, err := b.conn(ctx)
		if err != nil {
			return "", err
		}
		if err := c.SetBootDevice(ctx, sel, types.BIOSBootTypeEFI, persistent); err != nil {
			return "", err
		}
		return "boot device set", nil
	}), nil
}

func (b *IPMI) PlanFirmwareUpdate(context.Context, string) (*Plan, error) {
	return nil, fmt.Errorf("%w: firmware update needs Redfish", ErrUnsupported)
}

// Tasks implements Backend: IPMI has no task model.
func (b *IPMI) Tasks(context.Context) ([]Task, error) {
	return nil, fmt.Errorf("%w: tasks and jobs need Redfish", ErrUnsupported)
}

// Task implements Backend.
func (b *IPMI) Task(context.Context, string) (*Task, error) {
	return nil, fmt.Errorf("%w: tasks and jobs need Redfish", ErrUnsupported)
}

func (b *IPMI) PlanVirtualMedia(context.Context, string, bool) (*Plan, error) {
	return nil, fmt.Errorf("%w: virtual media needs Redfish", ErrUnsupported)
}

// retryReservation retries SDR/SEL walks that fail with completion code 0xC5
// ("reservation cancelled"): another client (a second rackhands, ipmitool,
// the BMC's own web UI) took a new repository reservation mid-walk. This is
// normal on a shared BMC and goes away on retry.
func retryReservation[T any](ctx context.Context, f func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for i := 0; i < 5; i++ {
		if v, err = f(); err == nil || !strings.Contains(strings.ToLower(err.Error()), "0xc5") {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, err
		case <-time.After(time.Duration(100*(i+1)) * time.Millisecond):
		}
	}
	return v, err
}

// Storage implements Backend. IPMI has no standard storage commands; a
// chassis drive fault bit is all it knows, and health already reports it.
func (b *IPMI) Storage(context.Context) (*StorageReport, error) {
	return nil, fmt.Errorf("%w: storage health (controllers, drives, volumes) needs Redfish; IPMI has no storage or RAID data", ErrUnsupported)
}
