// Package bmc normalises out-of-band management across vendors and
// protocols (Redfish with vendor OEM quirks, IPMI 2.0 fallback).
package bmc

import (
	"context"
	"errors"
	"strings"
)

// Health levels, worst last.
const (
	OK       = "OK"
	Warning  = "Warning"
	Critical = "Critical"
	Unknown  = "Unknown"
)

// ErrUnsupported means this backend (protocol or vendor) cannot do it.
var ErrUnsupported = errors.New("not supported by this BMC/protocol")

// Rank orders health strings so the worst can be chosen.
func Rank(h string) int {
	switch strings.ToLower(h) {
	case "ok":
		return 0
	case "warning":
		return 2
	case "critical":
		return 3
	case "":
		return -1
	}
	return 1 // unknown
}

// Worst returns the worst of the given health strings.
func Worst(hs ...string) string {
	best, out := -1, ""
	for _, h := range hs {
		if r := Rank(h); r > best {
			best, out = r, NormHealth(h)
		}
	}
	if out == "" {
		return Unknown
	}
	return out
}

// NormHealth maps vendor spellings onto OK/Warning/Critical/Unknown.
func NormHealth(h string) string {
	switch strings.ToLower(strings.TrimSpace(h)) {
	case "ok", "good", "normal":
		return OK
	case "warning", "degraded", "minor", "major":
		return Warning
	case "critical", "failed", "fatal", "nonrecoverable":
		return Critical
	case "":
		return ""
	}
	return Unknown
}

// Inventory is what the box is.
type Inventory struct {
	Target       string   `json:"target"`
	Via          string   `json:"via"`
	Vendor       string   `json:"vendor"`
	BMC          string   `json:"bmc,omitempty"`
	BMCFirmware  string   `json:"bmc_firmware,omitempty"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	Model        string   `json:"model,omitempty"`
	Serial       string   `json:"serial,omitempty"`
	SKU          string   `json:"sku,omitempty"`
	BIOS         string   `json:"bios,omitempty"`
	PowerState   string   `json:"power_state,omitempty"`
	Health       string   `json:"health,omitempty"`
	CPUCount     int      `json:"cpu_count,omitempty"`
	CPUModel     string   `json:"cpu_model,omitempty"`
	MemoryGiB    float64  `json:"memory_gib,omitempty"`
	Hostname     string   `json:"hostname,omitempty"`
	Account      *Account `json:"account,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

// Problem is one unhealthy component.
type Problem struct {
	Component string `json:"component"`
	Health    string `json:"health"`
	Detail    string `json:"detail,omitempty"`
}

// HealthReport is a rollup plus every component that is not OK.
type HealthReport struct {
	Target   string    `json:"target"`
	Via      string    `json:"via"`
	Vendor   string    `json:"vendor"`
	Overall  string    `json:"overall"`
	Problems []Problem `json:"problems"`
	Notes    []string  `json:"notes,omitempty"`
}

// Sensor is one reading.
type Sensor struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // temperature, fan, voltage, power, psu, other
	Reading   *float64 `json:"reading,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Health    string   `json:"health,omitempty"`
	State     string   `json:"state,omitempty"`
	UpperCrit *float64 `json:"upper_critical,omitempty"`
	LowerCrit *float64 `json:"lower_critical,omitempty"`
	Location  string   `json:"location,omitempty"`
}

// Event is one log entry (SEL, IML, Lifecycle, …).
type Event struct {
	ID       string `json:"id"`
	Time     string `json:"time,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Source   string `json:"source"`
	Code     string `json:"code,omitempty"`
}

// Firmware is one installed component version.
type Firmware struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	ID         string `json:"id,omitempty"`
	Updateable bool   `json:"updateable"`
}

// Boot is the boot configuration.
type Boot struct {
	Order          []string `json:"order"`
	OverrideTarget string   `json:"override_target,omitempty"`
	OverrideMode   string   `json:"override_enabled,omitempty"` // Disabled / Once / Continuous
	BootMode       string   `json:"boot_mode,omitempty"`        // UEFI / Legacy
	AllowedTargets []string `json:"allowed_targets,omitempty"`
}

// Plan describes a dangerous request before it is sent.
type Plan struct {
	Summary string         `json:"summary"`
	Method  string         `json:"method"`
	Path    string         `json:"path"`
	Body    map[string]any `json:"body,omitempty"`
	Current string         `json:"current,omitempty"`
	exec    func(context.Context) (string, error)
}

// Execute runs the planned request.
func (p *Plan) Execute(ctx context.Context) (string, error) {
	if p.exec == nil {
		return "", errors.New("plan has no executor")
	}
	return p.exec(ctx)
}

// NewPlan builds a plan with an executor (used by backends and tests).
func NewPlan(summary, method, path string, body map[string]any, current string, exec func(context.Context) (string, error)) *Plan {
	return &Plan{Summary: summary, Method: method, Path: path, Body: body, Current: current, exec: exec}
}

// Power actions accepted by PlanPower.
var PowerActions = []string{"on", "off", "force-off", "restart", "force-restart", "cycle", "nmi"}

// Backend is one way of talking to one BMC.
type Backend interface {
	Protocol() string // "redfish" or "ipmi"
	Vendor(ctx context.Context) string
	Inventory(ctx context.Context) (*Inventory, error)
	Health(ctx context.Context) (*HealthReport, error)
	Sensors(ctx context.Context) ([]Sensor, error)
	Events(ctx context.Context, limit int, allLogs bool) ([]Event, error)
	PowerState(ctx context.Context) (string, error)
	Firmware(ctx context.Context) ([]Firmware, error)
	Boot(ctx context.Context) (*Boot, error)

	PlanPower(ctx context.Context, action string) (*Plan, error)
	PlanBMCReset(ctx context.Context) (*Plan, error)
	PlanBootOverride(ctx context.Context, target string, persistent bool) (*Plan, error)
	PlanFirmwareUpdate(ctx context.Context, imageURI string) (*Plan, error)
	PlanVirtualMedia(ctx context.Context, image string, eject bool) (*Plan, error)
	Close(ctx context.Context)
}
