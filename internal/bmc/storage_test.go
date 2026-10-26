package bmc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Blackman99/rackhands/internal/redfish"
)

func findDrive(t *testing.T, s *StorageReport, name string) Drive {
	t.Helper()
	for _, d := range s.Drives {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no drive %q in %+v", name, s.Drives)
	return Drive{}
}

func findVolume(t *testing.T, s *StorageReport, name string) Volume {
	t.Helper()
	for _, v := range s.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no volume %q in %+v", name, s.Volumes)
	return Volume{}
}

func TestStoragePerVendor(t *testing.T) {
	cases := []struct {
		dir, source, health string
		ctrls, drives, vols int
		check               func(t *testing.T, s *StorageReport)
	}{
		{"dell", "Storage", Critical, 1, 5, 2, func(t *testing.T, s *StorageReport) {
			// AHCI.Embedded.2-1 (chipset SATA, nothing attached) is left out.
			if s.Controllers[0].ID != "0" || s.Controllers[0].Storage != "RAID.SL.3-1" || s.Controllers[0].Firmware != "52.21.0-4606" {
				t.Errorf("controller %+v", s.Controllers[0])
			}
			d := findDrive(t, s, "Physical Disk 0:1:2")
			if d.Health != Critical || d.RAIDStatus != "Failed" || d.Location != "Bay 2" || d.ID != "Disk.Bay.2:Enclosure.Internal.0-1:RAID.SL.3-1" {
				t.Errorf("failed drive %+v", d)
			}
			if !strings.HasPrefix(d.Why(), "failed · HDD 2.4 TB") || !strings.Contains(d.Why(), "S/N WBN0K2L7") {
				t.Errorf("why %q", d.Why())
			}
			v := findVolume(t, s, "pgdata")
			if v.Health != Warning || v.RAIDType != "RAID5" || v.Drives != 3 || v.RAIDStatus != "Degraded" || !strings.HasPrefix(v.Why(), "degraded · RAID5 4.8 TB") {
				t.Errorf("degraded volume %+v %q", v, v.Why())
			}
			if l := findDrive(t, s, "Physical Disk 0:1:0").LifeLeftPercent; l == nil || *l != 97 {
				t.Errorf("life left %v", l)
			}
		}},
		{"hpe", "Storage", Warning, 1, 4, 2, func(t *testing.T, s *StorageReport) {
			worn := findDrive(t, s, "Drive 0")
			if worn.BMCHealth != OK || worn.Health != Warning || !strings.HasPrefix(worn.Why(), "6% media life left") {
				t.Errorf("worn SSD %+v %q", worn, worn.Why())
			}
			pf := findDrive(t, s, "Drive 2")
			if pf.Health != Warning || pf.FailurePredicted == nil || !*pf.FailurePredicted || !strings.HasPrefix(pf.Why(), "failure predicted") {
				t.Errorf("predicted failure %+v", pf)
			}
			if findVolume(t, s, "Logical Drive 2").RAIDType != "RAID10" {
				t.Error("RAIDType")
			}
			if s.Controllers[0].Name != "HPE MR416i-a Gen10+" || s.Controllers[0].Firmware != "52.24.3-4948" {
				t.Errorf("controller from the Controllers collection: %+v", s.Controllers[0])
			}
		}},
		{"hpe-ilo4", "SmartStorage", Critical, 1, 5, 2, func(t *testing.T, s *StorageReport) {
			d := findDrive(t, s, "Drive 1I:1:4")
			if d.Health != Critical || d.RAIDStatus != "Failed" || d.CapacityBytes != 1144641<<20 {
				t.Errorf("failed Smart Array drive %+v", d)
			}
			if l := findDrive(t, s, "Drive 1I:1:1").LifeLeftPercent; l == nil || *l != 97 {
				t.Errorf("SSDEnduranceUtilizationPercentage 3 is 97%% left, got %v", l)
			}
			v := findVolume(t, s, "cache")
			if v.RAIDType != "RAID5" || v.Drives != 3 || v.Health != Warning || !strings.HasPrefix(v.Why(), "interim recovery mode") {
				t.Errorf("logical drive %+v %q", v, v.Why())
			}
			if s.Controllers[0].Firmware != "7.00" || len(s.Notes) == 0 {
				t.Errorf("controller %+v notes %v", s.Controllers[0], s.Notes)
			}
		}},
		{"lenovo", "Storage", OK, 1, 2, 1, func(t *testing.T, s *StorageReport) {
			if v := s.Volumes[0]; v.RAIDType != "RAID1" || v.Drives != 2 {
				t.Errorf("volume %+v", v)
			}
			if len(s.Problems()) != 0 {
				t.Errorf("healthy XCC has problems %+v", s.Problems())
			}
		}},
		{"supermicro", "Storage", Warning, 1, 4, 1, func(t *testing.T, s *StorageReport) {
			if d := findDrive(t, s, "Disk.3"); d.Operation != "Rebuild 37%" || d.Health != OK {
				t.Errorf("rebuilding drive under /Chassis %+v", d)
			}
			v := findVolume(t, s, "VD0")
			if v.RAIDType != "RAID5/6" || v.Operation != "Rebuild 37%" || v.Why() != "degraded, rebuild 37% · RAID5/6 12 TB · on HA-RAID" {
				t.Errorf("VolumeType-only volume %+v %q", v, v.Why())
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			b, srv := backend(t, c.dir)
			s, err := b.Storage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if s.Source != c.source || s.Health != c.health || len(s.Controllers) != c.ctrls || len(s.Drives) != c.drives || len(s.Volumes) != c.vols {
				t.Fatalf("source %s health %s: %d controllers, %d drives, %d volumes (%s)", s.Source, s.Health,
					len(s.Controllers), len(s.Drives), len(s.Volumes), s.Summary())
			}
			c.check(t, s)
			if srv.Count() != 0 {
				t.Fatalf("storage wrote to the BMC: %+v", srv.Writes)
			}
		})
	}
}

func TestHealthIncludesStorage(t *testing.T) {
	b, srv := backend(t, "dell")
	h, err := b.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Storage == nil || h.Storage.Summary() != "1 controller · 5 drives (1 Critical) · 2 volumes (1 Warning)" {
		t.Fatalf("storage section %+v", h.Storage)
	}
	got := problems(h)
	for _, w := range []string{"Critical:drive Physical Disk 0:1:2", "Warning:volume pgdata", "Critical:subsystem storage"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in %s", w, got)
		}
	}
	if srv.Count() != 0 {
		t.Fatal("health wrote to the BMC")
	}
	// A storage-only problem is enough to fail health: the iLO 4 box has
	// nothing else wrong.
	b4, _ := backend(t, "hpe-ilo4")
	h4, err := b4.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h4.Overall != Critical || !strings.Contains(problems(h4), "Critical:drive Drive 1I:1:4") {
		t.Fatalf("iLO 4: %s %s", h4.Overall, problems(h4))
	}
}

// fakeBMC serves a fixed set of Redfish resources.
func fakeBMC(t *testing.T, res map[string]string) *Redfish {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("storage sent %s %s", r.Method, r.URL.Path)
		}
		body, ok := res[strings.TrimSuffix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	c, err := redfish.New(redfish.Options{BaseURL: s.URL, Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	return NewRedfish(c, "box", "")
}

var fakeRoot = map[string]string{
	"/redfish/v1":         `{"@odata.id":"/redfish/v1/","Systems":{"@odata.id":"/redfish/v1/Systems"}}`,
	"/redfish/v1/Systems": `{"Members":[{"@odata.id":"/redfish/v1/Systems/1"}]}`,
}

func with(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range fakeRoot {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestStorageSimpleStorageFallback(t *testing.T) {
	// The DMTF public-rackmount1 shape: no Storage, one SimpleStorage with a
	// warning device and two empty bays.
	b := fakeBMC(t, with(map[string]string{
		"/redfish/v1/Systems/1": `{"@odata.id":"/redfish/v1/Systems/1","Id":"1","Status":{"Health":"OK"},
			"SimpleStorage":{"@odata.id":"/redfish/v1/Systems/1/SimpleStorage"}}`,
		"/redfish/v1/Systems/1/SimpleStorage": `{"Members":[{"@odata.id":"/redfish/v1/Systems/1/SimpleStorage/1"}]}`,
		"/redfish/v1/Systems/1/SimpleStorage/1": `{"Id":"1","Name":"Simple Storage Controller","Description":"System SATA",
			"Status":{"State":"Enabled","Health":"OK","HealthRollup":"Warning"},
			"Devices":[{"Name":"SATA Bay 1","Manufacturer":"Contoso","Model":"3000GT8","CapacityBytes":8000000000000,"Status":{"State":"Enabled","Health":"OK"}},
			{"Name":"SATA Bay 2","Manufacturer":"Contoso","Model":"3000GT7","CapacityBytes":4000000000000,"Status":{"State":"Enabled","Health":"Warning"}},
			{"Name":"SATA Bay 3","Status":{"State":"Absent"}}]}`,
	}))
	s, err := b.Storage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != "SimpleStorage" || s.Health != Warning || len(s.Drives) != 2 || len(s.Volumes) != 0 {
		t.Fatalf("%+v", s)
	}
	if s.Summary() != "1 controller · 2 drives (1 Warning)" {
		t.Errorf("summary %q", s.Summary())
	}
	if d := s.Drives[1]; d.Name != "SATA Bay 2" || d.Model != "Contoso 3000GT7" || d.Why() != "degraded · 4 TB Contoso 3000GT7 · on Simple Storage Controller (System SATA)" {
		t.Errorf("drive %+v %q", d, d.Why())
	}
	if !strings.Contains(strings.Join(s.Notes, " "), "no RAID volumes") {
		t.Errorf("notes %v", s.Notes)
	}
}

func TestStorageNotExposed(t *testing.T) {
	b := fakeBMC(t, with(map[string]string{
		"/redfish/v1/Systems/1": `{"@odata.id":"/redfish/v1/Systems/1","Id":"1","Status":{"Health":"OK"},
			"Storage":{"@odata.id":"/redfish/v1/Systems/1/Storage"}}`,
		// A Storage link that 404s (seen on Supermicro boards without the storage licence).
	}))
	s, err := b.Storage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != "" || s.Health != "" || s.Summary() != "not exposed by this BMC" || len(s.Notes) != 2 {
		t.Fatalf("%+v notes %q", s, s.Notes)
	}
	h, err := b.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Overall != OK {
		t.Fatalf("missing storage must not fail health: %s %s", h.Overall, problems(h))
	}
}

func TestStorageSkipsDuplicateNamesAndEmptyControllers(t *testing.T) {
	sub := func(id string) string {
		return `{"Id":"` + id + `","Name":"` + id + `","Status":{"Health":"OK"},
			"StorageControllers":[{"Name":"ctl ` + id + `","Status":{"Health":"OK","State":"Enabled"}}],
			"Drives":[{"@odata.id":"/redfish/v1/Systems/1/Storage/` + id + `/Drives/0"}]}`
	}
	d := `{"Id":"0","Name":"Drive 0","Status":{"Health":"OK","State":"Enabled"},"PredictedMediaLifeLeftPercent":9}`
	b := fakeBMC(t, with(map[string]string{
		"/redfish/v1/Systems/1": `{"@odata.id":"/redfish/v1/Systems/1","Id":"1","Storage":{"@odata.id":"/redfish/v1/Systems/1/Storage"}}`,
		"/redfish/v1/Systems/1/Storage": `{"Members":[{"@odata.id":"/redfish/v1/Systems/1/Storage/A"},{"@odata.id":"/redfish/v1/Systems/1/Storage/B"},
			{"@odata.id":"/redfish/v1/Systems/1/Storage/CPU.1"}]}`,
		"/redfish/v1/Systems/1/Storage/A":          sub("A"),
		"/redfish/v1/Systems/1/Storage/B":          sub("B"),
		"/redfish/v1/Systems/1/Storage/CPU.1":      `{"Id":"CPU.1","Name":"CPU.1","Status":{"Health":"OK"},"Drives":[]}`,
		"/redfish/v1/Systems/1/Storage/A/Drives/0": d,
		"/redfish/v1/Systems/1/Storage/B/Drives/0": d,
	}))
	s, err := b.Storage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Controllers) != 2 || s.Drives[0].Name != "Drive 0 (A)" || s.Drives[1].Name != "Drive 0 (B)" {
		t.Fatalf("%+v %+v", s.Controllers, s.Drives)
	}
	if s.Health != Warning || len(s.Problems()) != 2 {
		t.Fatalf("9%% life left is a warning: %s %+v", s.Health, s.Problems())
	}
}

func TestIPMIStorageNeedsRedfish(t *testing.T) {
	b := NewIPMI("sim", "127.0.0.1", 9, "u", "p", false, time.Second)
	_, err := b.Storage(ctx)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "needs Redfish") {
		t.Fatalf("got %v", err)
	}
}

func TestHumanBytesAndWords(t *testing.T) {
	for in, want := range map[int64]string{960e9: "960 GB", 2400e9: "2.4 TB", 1920e9: "1.92 TB", 4e12: "4 TB", 12e12: "12 TB", 0: "", 15360e9: "15.4 TB"} {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	if words("InterimRecoveryMode") != "interim recovery mode" || words("Failed") != "failed" {
		t.Error("words")
	}
}
