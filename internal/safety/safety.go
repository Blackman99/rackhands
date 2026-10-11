// Package safety gates dangerous BMC operations: an operation class must be
// enabled twice (at runtime and, for configured targets, per target), and
// every request is planned first and only executed with a short-lived,
// single-use confirmation bound to exactly that plan. Everything is audited.
package safety

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Op is a dangerous operation request.
type Op struct {
	Target string            `json:"target"`
	Class  string            `json:"class"` // power, boot, bmc-reset, firmware, virtual-media
	Action string            `json:"action"`
	Args   map[string]string `json:"args,omitempty"`
}

func (o Op) canonical() string {
	keys := make([]string, 0, len(o.Args))
	for k := range o.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%s\x00%s", o.Target, o.Class, o.Action)
	for _, k := range keys {
		fmt.Fprintf(&b, "\x00%s=%s", k, o.Args[k])
	}
	return b.String()
}

// Policy decides whether a class may run on a target.
type Policy struct {
	Runtime map[string]bool // enabled by --allow / RACKHANDS_ALLOW
}

// ParseAllow turns "power,boot" (or "all") into a set.
func ParseAllow(list []string) map[string]bool {
	m := map[string]bool{}
	for _, item := range list {
		for _, c := range strings.Split(item, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				m[c] = true
			}
		}
	}
	return m
}

func enabled(set map[string]bool, class string) bool { return set["all"] || set[class] }

// ErrNotAllowed is returned when a class is not enabled.
var ErrNotAllowed = errors.New("operation not allowed")

// Check enforces the two keys. targetAllow is nil for ad-hoc targets given
// on the command line (the operator named the host explicitly).
func (p Policy) Check(class string, targetName string, targetAllow []string, configured bool) error {
	if !enabled(p.Runtime, class) {
		return fmt.Errorf("%w: %q is off; start with --allow %s (dangerous operations are disabled by default)", ErrNotAllowed, class, class)
	}
	if configured && !enabled(ParseAllow(targetAllow), class) {
		return fmt.Errorf("%w: target %q does not list %q under allow: in the config file", ErrNotAllowed, targetName, class)
	}
	return nil
}

// AnyEnabled reports whether any dangerous class is enabled at runtime.
func (p Policy) AnyEnabled() bool { return len(p.Runtime) > 0 }

// Enabled reports whether a class is enabled at runtime.
func (p Policy) Enabled(class string) bool { return enabled(p.Runtime, class) }

// Tokens issues and redeems confirmation tokens.
type Tokens struct {
	key  []byte
	ttl  time.Duration
	now  func() time.Time
	mu   sync.Mutex
	used map[string]time.Time
}

// NewTokens creates an issuer with a random per-process key.
func NewTokens(ttl time.Duration) *Tokens {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &Tokens{key: k, ttl: ttl, now: time.Now, used: map[string]time.Time{}}
}

func (t *Tokens) mac(op Op, exp int64) string {
	h := hmac.New(sha256.New, t.key)
	fmt.Fprintf(h, "%d\x00%s", exp, op.canonical())
	return hex.EncodeToString(h.Sum(nil))[:20]
}

// Issue returns a token for exactly this op.
func (t *Tokens) Issue(op Op) (string, time.Time) {
	exp := t.now().Add(t.ttl)
	return fmt.Sprintf("rh1-%d-%s", exp.Unix(), t.mac(op, exp.Unix())), exp
}

// Redeem checks the token matches op, is unexpired and unused, and burns it.
func (t *Tokens) Redeem(op Op, token string) error {
	parts := strings.Split(token, "-")
	if len(parts) != 3 || parts[0] != "rh1" {
		return errors.New("malformed confirm_token")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return errors.New("malformed confirm_token")
	}
	if !hmac.Equal([]byte(parts[2]), []byte(t.mac(op, exp))) {
		return errors.New("confirm_token does not match this exact request (target, action and arguments must be identical to the plan)")
	}
	if t.now().Unix() > exp {
		return errors.New("confirm_token expired; request a new plan")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, dup := t.used[token]; dup {
		return errors.New("confirm_token already used; request a new plan")
	}
	t.used[token] = time.Unix(exp, 0)
	for k, e := range t.used {
		if t.now().After(e) {
			delete(t.used, k)
		}
	}
	return nil
}

// Audit appends JSON lines to a file. A nil Audit is a no-op.
type Audit struct {
	mu   sync.Mutex
	path string
}

// DefaultAuditPath is ~/.local/state/rackhands/audit.jsonl (or XDG_STATE_HOME).
func DefaultAuditPath() string {
	if p := os.Getenv("RACKHANDS_AUDIT_LOG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "rackhands", "audit.jsonl")
}

// NewAudit opens (lazily) an audit log at path; "" or "off" disables it.
func NewAudit(path string) *Audit {
	if path == "" || path == "off" {
		return nil
	}
	return &Audit{path: path}
}

// Path returns the file path.
func (a *Audit) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// Record writes one entry.
func (a *Audit) Record(actor, phase string, op Op, detail map[string]any) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "actor": actor, "phase": phase, "op": op}
	for k, v := range detail {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	_, err = f.Write(append(b, '\n'))
	return err
}
