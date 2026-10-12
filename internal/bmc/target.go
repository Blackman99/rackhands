package bmc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Blackman99/rackhands/internal/config"
	"github.com/Blackman99/rackhands/internal/redfish"
)

// Conn is a resolved target: Redfish first, IPMI as fallback.
type Conn struct {
	Name    string
	Primary Backend // may be nil (IPMI-only target)
	Second  Backend // may be nil (no fallback)
	Allow   []string
}

// Open builds backends for a config target. Nothing is contacted yet.
func Open(t *config.Target) (*Conn, error) {
	pw, err := t.Secret()
	if err != nil {
		return nil, err
	}
	c := &Conn{Name: t.Name, Allow: t.Allow}
	if t.Redfish != "" {
		cl, err := redfish.New(redfish.Options{BaseURL: t.Redfish, Username: t.Username, Password: pw,
			Insecure: t.InsecureTLS(), CAFile: t.CAFile, TLSFingerprint: t.TLSFingerprint,
			Session: t.Session, Timeout: t.TimeoutDur()})
		if err != nil {
			return nil, err
		}
		c.Primary = NewRedfish(cl, t.Name, t.Vendor)
	}
	if t.IPMI != "" {
		h, p := t.IPMIAddr()
		ib := NewIPMI(t.Name, h, p, t.Username, pw, t.IPMILan15, t.TimeoutDur()/2)
		if c.Primary == nil {
			c.Primary = ib
		} else {
			c.Second = ib
		}
	}
	return c, nil
}

// Close releases sessions.
func (c *Conn) Close(ctx context.Context) {
	if c.Primary != nil {
		c.Primary.Close(ctx)
	}
	if c.Second != nil {
		c.Second.Close(ctx)
	}
}

// shouldFallback: the Redfish side is down, absent, or cannot do this.
func shouldFallback(err error) bool {
	return errors.Is(err, redfish.ErrUnreachable) || errors.Is(err, ErrUnsupported) ||
		redfish.IsStatus(err, http.StatusNotFound) || redfish.IsStatus(err, http.StatusServiceUnavailable) ||
		redfish.IsStatus(err, http.StatusNotImplemented)
}

// Via describes which protocol answered and why.
type Via struct {
	Protocol string `json:"protocol"`
	Fallback string `json:"fallback_reason,omitempty"`
}

func (v Via) String() string {
	if v.Fallback == "" {
		return v.Protocol
	}
	return v.Protocol + " (fallback: " + v.Fallback + ")"
}

// Call runs f on the primary backend and falls back to IPMI when Redfish is
// unreachable or cannot do the operation.
func Call[T any](ctx context.Context, c *Conn, f func(Backend) (T, error)) (T, Via, error) {
	var zero T
	if c.Primary == nil {
		return zero, Via{}, errors.New("target has no backend")
	}
	out, err := f(c.Primary)
	if err == nil {
		return out, Via{Protocol: c.Primary.Protocol()}, nil
	}
	if c.Second == nil || !shouldFallback(err) {
		return zero, Via{Protocol: c.Primary.Protocol()}, err
	}
	reason := shortErr(err)
	out2, err2 := f(c.Second)
	if err2 != nil {
		return zero, Via{Protocol: c.Second.Protocol(), Fallback: reason}, fmt.Errorf("redfish: %v; ipmi: %w", reason, err2)
	}
	return out2, Via{Protocol: c.Second.Protocol(), Fallback: reason}, nil
}

func shortErr(err error) string {
	s := err.Error()
	if errors.Is(err, redfish.ErrUnreachable) {
		// "redfish service unreachable: Get \"https://…\": dial tcp …: connect: connection refused"
		if i := strings.LastIndex(s, ": "); i > 0 {
			s = "redfish unreachable (" + s[i+2:] + ")"
		}
	}
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
