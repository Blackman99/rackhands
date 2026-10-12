// Package config loads the BMC inventory file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Blackman99/rackhands/internal/redfish"
	"gopkg.in/yaml.v3"
)

// Target is one BMC.
type Target struct {
	Name           string   `yaml:"name"`
	Redfish        string   `yaml:"redfish"`    // https://10.0.0.5 (empty = IPMI only)
	IPMI           string   `yaml:"ipmi"`       // 10.0.0.5[:623] fallback (empty = none)
	IPMILan15      bool     `yaml:"ipmi_lan15"` // IPMI 1.5 instead of 2.0 lanplus
	Username       string   `yaml:"username"`
	Password       string   `yaml:"password"` // discouraged; prefer password_env / password_file
	PasswordEnv    string   `yaml:"password_env"`
	PasswordFile   string   `yaml:"password_file"`
	Insecure       *bool    `yaml:"insecure"` // accept self-signed BMC certificates
	CAFile         string   `yaml:"ca_file"`
	TLSFingerprint string   `yaml:"tls_fingerprint"` // SHA-256 of leaf cert; safer than insecure: true
	Session        bool     `yaml:"session_auth"`    // X-Auth-Token sessions instead of basic auth
	Vendor         string   `yaml:"vendor"`          // auto|dell|hpe|lenovo|supermicro|generic
	Allow          []string `yaml:"allow"`           // dangerous classes permitted on this target
	Tags           []string `yaml:"tags"`
	Timeout        string   `yaml:"timeout"`
}

// File is the whole config.
type File struct {
	Defaults Target   `yaml:"defaults"`
	Targets  []Target `yaml:"targets"`
	AuditLog string   `yaml:"audit_log"`
	path     string
}

// DefaultPath is where Load looks when no path is given.
func DefaultPath() string {
	if p := os.Getenv("RACKHANDS_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "rackhands.yaml"
	}
	return filepath.Join(dir, "rackhands", "targets.yaml")
}

// Load reads and validates a config file. A missing default file yields an
// empty config (ad-hoc --host use still works).
func Load(path string) (*File, error) {
	explicit := path != ""
	if !explicit {
		path = DefaultPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit && os.Getenv("RACKHANDS_CONFIG") == "" {
			return &File{path: path}, nil
		}
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.path = path
	seen := map[string]bool{}
	for i := range f.Targets {
		t := &f.Targets[i]
		if t.Name == "" {
			return nil, fmt.Errorf("%s: target #%d has no name", path, i+1)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("%s: duplicate target %q", path, t.Name)
		}
		seen[t.Name] = true
		if t.Redfish == "" && t.IPMI == "" {
			return nil, fmt.Errorf("%s: target %q needs redfish: and/or ipmi:", path, t.Name)
		}
		f.applyDefaults(t)
		if err := ValidateClasses(t.Allow); err != nil {
			return nil, fmt.Errorf("%s: target %q: %w", path, t.Name, err)
		}
		if t.TLSFingerprint != "" {
			norm, err := redfish.NormalizeFingerprint(t.TLSFingerprint)
			if err != nil {
				return nil, fmt.Errorf("%s: target %q: %w", path, t.Name, err)
			}
			t.TLSFingerprint = norm
		}
	}
	return &f, nil
}

func (f *File) applyDefaults(t *Target) {
	d := f.Defaults
	if t.Username == "" {
		t.Username = d.Username
	}
	if t.Password == "" && t.PasswordEnv == "" && t.PasswordFile == "" {
		t.Password, t.PasswordEnv, t.PasswordFile = d.Password, d.PasswordEnv, d.PasswordFile
	}
	if t.Insecure == nil {
		t.Insecure = d.Insecure
	}
	if t.CAFile == "" {
		t.CAFile = d.CAFile
	}
	if t.TLSFingerprint == "" {
		t.TLSFingerprint = d.TLSFingerprint
	}
	if !t.Session {
		t.Session = d.Session
	}
	if t.Vendor == "" {
		t.Vendor = d.Vendor
	}
	if t.Timeout == "" {
		t.Timeout = d.Timeout
	}
	if !t.IPMILan15 {
		t.IPMILan15 = d.IPMILan15
	}
}

// Path is the file this config came from.
func (f *File) Path() string { return f.path }

// Find returns the named target.
func (f *File) Find(name string) (*Target, bool) {
	for i := range f.Targets {
		if f.Targets[i].Name == name {
			return &f.Targets[i], true
		}
	}
	return nil, false
}

// Select resolves names, "all", or "tag:x" into targets.
func (f *File) Select(sel []string) ([]*Target, error) {
	var out []*Target
	add := func(t *Target) {
		for _, o := range out {
			if o == t {
				return
			}
		}
		out = append(out, t)
	}
	for _, s := range sel {
		switch {
		case s == "all":
			for i := range f.Targets {
				add(&f.Targets[i])
			}
		case strings.HasPrefix(s, "tag:"):
			tag, n := strings.TrimPrefix(s, "tag:"), 0
			for i := range f.Targets {
				for _, tt := range f.Targets[i].Tags {
					if tt == tag {
						add(&f.Targets[i])
						n++
					}
				}
			}
			if n == 0 {
				return nil, fmt.Errorf("no target has tag %q", tag)
			}
		default:
			t, ok := f.Find(s)
			if !ok {
				return nil, fmt.Errorf("unknown target %q (see `rackhands targets`)", s)
			}
			add(t)
		}
	}
	return out, nil
}

// Secret returns the password from env, file or inline value.
func (t *Target) Secret() (string, error) {
	switch {
	case t.PasswordEnv != "":
		v, ok := os.LookupEnv(t.PasswordEnv)
		if !ok {
			return "", fmt.Errorf("target %q: environment variable %s is not set", t.Name, t.PasswordEnv)
		}
		return v, nil
	case t.PasswordFile != "":
		b, err := os.ReadFile(t.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("target %q: %w", t.Name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return t.Password, nil
}

// InsecureTLS reports whether self-signed certs are accepted (default true:
// almost every BMC ships one). Prefer tls_fingerprint: (pin) or ca_file with
// insecure: false in production. When tls_fingerprint is set, the Redfish
// client pins the leaf SHA-256 and ignores this flag for trust decisions.
func (t *Target) InsecureTLS() bool { return t.Insecure == nil || *t.Insecure }

// TimeoutDur parses timeout (default 20s).
func (t *Target) TimeoutDur() time.Duration {
	if d, err := time.ParseDuration(t.Timeout); err == nil && d > 0 {
		return d
	}
	return 20 * time.Second
}

// IPMIAddr splits host[:port].
func (t *Target) IPMIAddr() (string, int) {
	h := t.IPMI
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i+1:], "]") {
		if p, err := strconv.Atoi(h[i+1:]); err == nil {
			return strings.Trim(h[:i], "[]"), p
		}
	}
	return strings.Trim(h, "[]"), 623
}

// Dangerous operation classes. Reads need no class.
var Classes = []string{"power", "boot", "bmc-reset", "firmware", "virtual-media"}

// ValidateClasses rejects unknown names.
func ValidateClasses(cs []string) error {
	for _, c := range cs {
		ok := c == "all"
		for _, k := range Classes {
			if c == k {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("unknown operation class %q (want %s or all)", c, strings.Join(Classes, ", "))
		}
	}
	return nil
}

// Names returns sorted target names.
func (f *File) Names() []string {
	var n []string
	for _, t := range f.Targets {
		n = append(n, t.Name)
	}
	sort.Strings(n)
	return n
}
