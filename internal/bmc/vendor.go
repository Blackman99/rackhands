package bmc

import (
	"strings"

	"github.com/Blackman99/rackhands/internal/redfish"
)

// Profile captures what differs between BMC families. Everything not listed
// here follows the DMTF schema; quirks are kept in one table on purpose so a
// new vendor is a small, reviewable diff.
type Profile struct {
	Vendor string // Dell, HPE, Lenovo, Supermicro, Generic
	BMC    string // iDRAC, iLO, XCC, BMC
	// Log services to read for "events", by Id, best first. Hardware event
	// logs only; audit/lifecycle logs need --all-logs.
	Logs []string
	// HideFirmware filters firmware inventory noise (Dell lists the
	// previous and staged copy of every component).
	HideFirmware func(id string) bool
	// BMCResetNoBody: iLO rejects a ResetType parameter on Manager.Reset.
	BMCResetNoBody bool
	// SeverityKey: where the vendor keeps a richer severity than Severity.
	SeverityKey string
	// Notes printed with inventory, e.g. known gaps.
	VMediaUnsupported string
}

var profiles = map[string]Profile{
	"Dell": {
		Vendor: "Dell", BMC: "iDRAC",
		Logs: []string{"Sel", "SEL"},
		HideFirmware: func(id string) bool {
			return strings.HasPrefix(id, "Previous-") || strings.HasPrefix(id, "Available-")
		},
	},
	"HPE": {
		Vendor: "HPE", BMC: "iLO",
		Logs:           []string{"IML", "IEL"},
		BMCResetNoBody: true,
		SeverityKey:    "Oem.Hpe.Severity",
	},
	"Lenovo": {
		Vendor: "Lenovo", BMC: "XCC",
		Logs: []string{"PlatformLog", "SEL", "EventLog"},
	},
	"Supermicro": {
		Vendor: "Supermicro", BMC: "BMC",
		Logs: []string{"Log1", "SEL"},
	},
	"Generic": {
		Vendor: "Generic", BMC: "BMC",
		Logs: []string{"SEL", "Sel", "Log", "Log1", "EventLog"},
	},
}

// Profiles lists supported vendor names.
func Profiles() []string { return []string{"Dell", "HPE", "Lenovo", "Supermicro", "Generic"} }

// ProfileFor returns the profile for a vendor name (case-insensitive).
func ProfileFor(name string) Profile {
	for k, p := range profiles {
		if strings.EqualFold(k, name) {
			return p
		}
	}
	return profiles["Generic"]
}

// DetectVendor guesses the vendor from the service root and manager.
func DetectVendor(root, manager redfish.Object) string {
	cands := []string{root.Str("Vendor"), root.Str("Product"), manager.Str("Manufacturer"), manager.Str("Model")}
	for k := range root.Obj("Oem") {
		cands = append(cands, k)
	}
	for k := range manager.Obj("Oem") {
		cands = append(cands, k)
	}
	for _, c := range cands {
		l := strings.ToLower(c)
		switch {
		case strings.Contains(l, "dell") || strings.Contains(l, "idrac"):
			return "Dell"
		case l == "hp" || strings.Contains(l, "hpe") || strings.Contains(l, "hewlett") || strings.Contains(l, "ilo"):
			return "HPE"
		case strings.Contains(l, "lenovo") || strings.Contains(l, "xclarity") || strings.Contains(l, "xcc"):
			return "Lenovo"
		case strings.Contains(l, "supermicro"):
			return "Supermicro"
		}
	}
	return "Generic"
}
