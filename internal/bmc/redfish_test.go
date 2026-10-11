package bmc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Blackman99/rackhands/internal/redfish"
	"github.com/Blackman99/rackhands/internal/testbmc"
)

func backend(t *testing.T, vendor string) (*Redfish, *testbmc.Server) {
	t.Helper()
	s := testbmc.Start(t, vendor)
	c, err := redfish.New(redfish.Options{BaseURL: s.URL, Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	return NewRedfish(c, vendor+"-box", ""), s
}

var ctx = context.Background()

func TestVendorDetectionAndInventory(t *testing.T) {
	cases := []struct{ dir, vendor, model, serial, bmcfw string }{
		{"dell", "Dell", "PowerEdge R750", "CNWS3001A0012", "7.00.00.172"},
		{"hpe", "HPE", "ProLiant DL380 Gen10 Plus", "MXQ12345AB", "iLO 5 v3.08"},
		{"lenovo", "Lenovo", "ThinkSystem SR650 V2 -[7Z73CTO1WW]-", "J30AB123", "UHC830D 2.10"},
		{"supermicro", "Supermicro", "SYS-6019U-TR4", "S41234567890", "01.73.12"},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			b, _ := backend(t, c.dir)
			inv, err := b.Inventory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if inv.Vendor != c.vendor || inv.Model != c.model || inv.Serial != c.serial || inv.BMCFirmware != c.bmcfw {
				t.Fatalf("got %+v", inv)
			}
			if inv.CPUCount != 2 || inv.MemoryGiB == 0 || inv.PowerState != "On" {
				t.Fatalf("summary missing: %+v", inv)
			}
		})
	}
}

func TestHPEManagerTypeFromServiceRoot(t *testing.T) {
	b, _ := backend(t, "hpe")
	inv, _ := b.Inventory(ctx)
	if inv.BMC != "iLO 5" {
		t.Fatalf("BMC label %q", inv.BMC)
	}
}

func problems(h *HealthReport) string {
	var s []string
	for _, p := range h.Problems {
		s = append(s, p.Health+":"+p.Component)
	}
	return strings.Join(s, "|")
}

func TestHealth(t *testing.T) {
	cases := []struct {
		dir, overall string
		want         []string
	}{
		{"dell", Critical, []string{"Critical:subsystem power supplies", "Critical:psu PS2 Status"}},
		{"hpe", Critical, []string{"Warning:subsystem Fans", "Critical:fan Fan 3"}},
		{"lenovo", OK, nil},
		{"supermicro", Warning, []string{"Warning:temperature CPU1 Temp", "Warning:voltage VBAT"}},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			b, _ := backend(t, c.dir)
			h, err := b.Health(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if h.Overall != c.overall {
				t.Fatalf("overall %s, problems %s", h.Overall, problems(h))
			}
			got := problems(h)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %s", w, got)
				}
			}
			if c.want == nil && len(h.Problems) != 0 {
				t.Errorf("unexpected problems %s", got)
			}
		})
	}
}

func TestSensorsNormalisation(t *testing.T) {
	b, _ := backend(t, "hpe")
	ss, err := b.Sensors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Name == "04-P1 DIMM 1-6" {
			t.Error("absent sensor should be skipped")
		}
		if s.Name == "02-CPU 1" && s.UpperCrit != nil {
			t.Error("iLO threshold 0 must mean no threshold")
		}
		if s.Kind == "fan" && s.Unit != "%" {
			t.Errorf("fan unit %q", s.Unit)
		}
	}
	// Lenovo exposes only the newer Sensors collection.
	lb, _ := backend(t, "lenovo")
	ls, err := lb.Sensors(ctx)
	if err != nil || len(ls) != 5 {
		t.Fatalf("lenovo sensors: %v %d", err, len(ls))
	}
	kinds := map[string]bool{}
	for _, s := range ls {
		kinds[s.Kind] = true
		if s.Kind == "temperature" && s.Unit != "C" {
			t.Errorf("unit %q not normalised", s.Unit)
		}
	}
	for _, k := range []string{"temperature", "fan", "voltage", "power"} {
		if !kinds[k] {
			t.Errorf("missing kind %s", k)
		}
	}
}

func TestEventsPickVendorLog(t *testing.T) {
	b, _ := backend(t, "dell")
	evs, err := b.Events(ctx, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 || !strings.HasPrefix(evs[0].Source, "Sel") || evs[0].Code != "PSU0003" {
		t.Fatalf("dell events: %+v", evs)
	}
	if evs[0].Time != "2026-10-09T08:12:44Z" {
		t.Errorf("time not normalised to UTC: %s", evs[0].Time)
	}
	all, _ := b.Events(ctx, 10, true)
	if len(all) != 5 {
		t.Errorf("all logs: %d", len(all))
	}

	h, _ := backend(t, "hpe")
	evs, err = h.Events(ctx, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Severity != Warning || evs[1].Severity != Critical || !strings.HasPrefix(evs[0].Source, "IML") {
		t.Fatalf("hpe IML (Caution->Warning, non-inline members): %+v", evs)
	}

	l, _ := backend(t, "lenovo")
	evs, _ = l.Events(ctx, 5, false)
	if len(evs) != 2 || evs[1].Message != "FQXSPMA0025I DIMM 14" {
		t.Fatalf("lenovo MessageId fallback: %+v", evs)
	}
}

func TestFirmwareHidesDellCopies(t *testing.T) {
	b, _ := backend(t, "dell")
	fw, err := b.Firmware(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fw {
		if strings.HasPrefix(f.ID, "Previous-") || strings.HasPrefix(f.ID, "Available-") {
			t.Errorf("should hide %s", f.ID)
		}
	}
	if len(fw) != 5 {
		t.Errorf("got %d entries", len(fw))
	}
}

func TestBootOrderNames(t *testing.T) {
	b, _ := backend(t, "dell")
	bo, err := b.Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bo.Order) != 3 || !strings.HasPrefix(bo.Order[1], "PXE Device 1") {
		t.Fatalf("%+v", bo)
	}
}

func TestPowerPlans(t *testing.T) {
	d, ds := backend(t, "dell")
	p, err := d.PlanPower(ctx, "cycle")
	if err != nil || p.Body["ResetType"] != "PowerCycle" {
		t.Fatalf("dell cycle: %v %+v", err, p)
	}
	if ds.Count() != 0 {
		t.Fatal("planning must not write")
	}
	if _, err := p.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	w, _ := ds.Last()
	if w.Method != "POST" || !strings.HasSuffix(w.Path, "ComputerSystem.Reset") || w.Body["ResetType"] != "PowerCycle" {
		t.Fatalf("wrote %+v", w)
	}

	h, _ := backend(t, "hpe")
	if _, err := h.PlanPower(ctx, "cycle"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("iLO has no PowerCycle: %v", err)
	}
	if p, _ := h.PlanPower(ctx, "off"); p.Body["ResetType"] != "GracefulShutdown" {
		t.Fatal("off should be graceful")
	}
	if _, err := h.PlanPower(ctx, "explode"); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestBMCResetQuirks(t *testing.T) {
	h, hs := backend(t, "hpe")
	p, err := h.PlanBMCReset(ctx)
	if err != nil || len(p.Body) != 0 {
		t.Fatalf("iLO reset must have empty body: %v %+v", err, p)
	}
	_, _ = p.Execute(ctx)
	if w, _ := hs.Last(); len(w.Body) != 0 {
		t.Fatalf("sent %+v", w.Body)
	}
	d, _ := backend(t, "dell")
	p, _ = d.PlanBMCReset(ctx)
	if p.Body["ResetType"] != "GracefulRestart" {
		t.Fatalf("dell body %+v", p.Body)
	}
}

func TestBootOverrideIfMatch(t *testing.T) {
	l, ls := backend(t, "lenovo")
	ls.RequireIfMatch = true
	p, err := l.PlanBootOverride(ctx, "pxe", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Execute(ctx); err != nil {
		t.Fatalf("PATCH with If-Match should pass: %v", err)
	}
	w, _ := ls.Last()
	boot := w.Body["Boot"].(map[string]any)
	if w.IfMatch != `W/"6a1b2c3d"` || boot["BootSourceOverrideTarget"] != "Pxe" || boot["BootSourceOverrideEnabled"] != "Once" {
		t.Fatalf("%+v", w)
	}
	if _, err := l.PlanBootOverride(ctx, "UefiHttp", false); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("XCC does not allow UefiHttp here: %v", err)
	}
	if _, err := l.PlanBootOverride(ctx, "floppy-disk", false); err == nil {
		t.Fatal("unknown target accepted")
	}
}

func TestVirtualMediaVendorPaths(t *testing.T) {
	h, _ := backend(t, "hpe")
	p, err := h.PlanVirtualMedia(ctx, "http://10.0.0.1/ubuntu.iso", false)
	if err != nil || !strings.Contains(p.Path, "HpeiLOVirtualMedia.InsertVirtualMedia") {
		t.Fatalf("iLO OEM insert: %v %+v", err, p)
	}
	d, _ := backend(t, "dell")
	p, err = d.PlanVirtualMedia(ctx, "http://10.0.0.1/ubuntu.iso", false)
	if err != nil || !strings.HasSuffix(p.Path, "VirtualMedia.InsertMedia") || p.Body["WriteProtected"] != true {
		t.Fatalf("standard insert: %v %+v", err, p)
	}
	if p, err := d.PlanVirtualMedia(ctx, "", true); err != nil || !strings.HasSuffix(p.Path, "EjectMedia") {
		t.Fatalf("eject: %v", err)
	}
	s, _ := backend(t, "supermicro")
	if _, err := s.PlanVirtualMedia(ctx, "http://10.0.0.1/x.iso", false); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "X11") {
		t.Fatalf("supermicro X11: %v", err)
	}
	if _, err := d.PlanVirtualMedia(ctx, "not a url", false); err == nil {
		t.Fatal("bad URL accepted")
	}
}

func TestFirmwareUpdate(t *testing.T) {
	d, ds := backend(t, "dell")
	p, err := d.PlanFirmwareUpdate(ctx, "https://repo.example/BIOS_1.14.1.EXE")
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Execute(ctx)
	if err != nil || !strings.Contains(res, "JID_001") {
		t.Fatalf("%v %s", err, res)
	}
	if w, _ := ds.Last(); w.Body["ImageURI"] != "https://repo.example/BIOS_1.14.1.EXE" {
		t.Fatal("ImageURI not sent")
	}
	s, _ := backend(t, "supermicro")
	if _, err := s.PlanFirmwareUpdate(ctx, "https://x/y.bin"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("no SimpleUpdate: %v", err)
	}
	if _, err := d.PlanFirmwareUpdate(ctx, "/local/file.exe"); err == nil {
		t.Fatal("local path accepted")
	}
}

func TestUnreachable(t *testing.T) {
	c, _ := redfish.New(redfish.Options{BaseURL: "http://127.0.0.1:1"})
	b := NewRedfish(c, "dead", "")
	if _, err := b.Inventory(ctx); !errors.Is(err, redfish.ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}

func TestWorstAndNorm(t *testing.T) {
	if Worst("OK", "Warning", "", "Critical") != Critical || Worst("OK", "") != OK || Worst() != Unknown {
		t.Fatal("Worst")
	}
	if NormHealth("Degraded") != Warning || eventSeverity("Caution") != Warning || eventSeverity("Repaired") != OK {
		t.Fatal("norm")
	}
}

type fakeBackend struct {
	Backend
	proto string
	err   error
}

func (f fakeBackend) Protocol() string { return f.proto }
func (f fakeBackend) PowerState(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "On", nil
}

func TestCallFallsBack(t *testing.T) {
	c := &Conn{Primary: fakeBackend{proto: "redfish", err: redfish.ErrUnreachable}, Second: fakeBackend{proto: "ipmi"}}
	st, via, err := Call(ctx, c, func(b Backend) (string, error) { return b.PowerState(ctx) })
	if err != nil || st != "On" || via.Protocol != "ipmi" || via.Fallback == "" {
		t.Fatalf("%v %s %+v", err, st, via)
	}
	// A real answer from Redfish (e.g. 401) must not silently fall back.
	c.Primary = fakeBackend{proto: "redfish", err: &redfish.HTTPError{Status: 401}}
	if _, _, err := Call(ctx, c, func(b Backend) (string, error) { return b.PowerState(ctx) }); err == nil {
		t.Fatal("401 should not fall back")
	}
}
