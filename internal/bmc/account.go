package bmc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Blackman99/rackhands/internal/redfish"
	"github.com/bougou/go-ipmi/pkg/types"
)

// Account is the effective privilege of the BMC account rackhands logs in
// with. It is read-only information: rackhands never changes BMC accounts.
type Account struct {
	User string `json:"user,omitempty"`
	// Role is ReadOnly / Operator / Administrator (Redfish), User / Operator /
	// Administrator (IPMI), a custom role id, or Unknown.
	Role string `json:"role"`
	// CanControl reports whether the account itself could power off, reset
	// or reconfigure the machine. nil when that could not be determined.
	CanControl *bool    `json:"can_control,omitempty"`
	Privileges []string `json:"privileges,omitempty"`
	Source     string   `json:"source,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	Warning    string   `json:"warning,omitempty"`
}

// AccountReader is implemented by backends that can report the effective
// privilege of their own login.
type AccountReader interface {
	Account(ctx context.Context) (*Account, error)
}

// RoleUnknown is reported when the privilege could not be determined.
const RoleUnknown = "Unknown"

func boolp(b bool) *bool { return &b }

// Redfish privileges that let an account change the machine.
var controlPrivileges = []string{"ConfigureComponents", "ConfigureManager", "ConfigureUsers"}

func canControl(privs []string) bool {
	for _, p := range privs {
		for _, c := range controlPrivileges {
			if strings.EqualFold(p, c) {
				return true
			}
		}
	}
	return false
}

// Check sets Warning when the account holds power the target never needs:
// no dangerous class is allowed for it, yet the login could power it off.
func (a *Account) Check(allow []string) {
	if a == nil || a.CanControl == nil || !*a.CanControl || len(allow) > 0 {
		return
	}
	a.Warning = fmt.Sprintf("logs in as %s, which can power off or reconfigure this machine, but no dangerous class is allowed for it; "+
		"use a read-only BMC account (Redfish ReadOnly role, IPMI User privilege)", a.Role)
}

// InventoryWithAccount reads the inventory and adds the effective privilege
// of the configured account, warning when it is stronger than needed.
func InventoryWithAccount(ctx context.Context, c *Conn) (*Inventory, Via, error) {
	inv, via, err := Call(ctx, c, func(b Backend) (*Inventory, error) { return b.Inventory(ctx) })
	if err != nil || inv == nil {
		return inv, via, err
	}
	b := c.Primary
	if c.Second != nil && via.Protocol == c.Second.Protocol() && via.Fallback != "" {
		b = c.Second
	}
	if ar, ok := b.(AccountReader); ok {
		acc, aerr := ar.Account(ctx)
		if aerr != nil {
			acc = &Account{Role: RoleUnknown, Detail: shortErr(aerr)}
		}
		acc.Check(c.Allow)
		inv.Account = acc
		if acc.Warning != "" {
			inv.Notes = append(inv.Notes, "WARNING: "+acc.Warning)
		}
	}
	return inv, via, nil
}

// Account finds the configured user in AccountService and reports its role.
// Best effort: BMCs that hide AccountService from the account report Unknown.
func (r *Redfish) Account(ctx context.Context) (*Account, error) {
	r.load(ctx)
	if r.loadErr != nil {
		return nil, r.loadErr
	}
	user := r.c.Username()
	a := &Account{User: user, Role: RoleUnknown}
	asRef := r.root.Ref("AccountService")
	switch {
	case user == "":
		a.Detail = "no username configured"
		return a, nil
	case asRef == "":
		a.Detail = "BMC exposes no AccountService"
		return a, nil
	}
	as, err := r.c.Get(ctx, asRef)
	if err != nil {
		if errors.Is(err, redfish.ErrUnreachable) {
			return nil, err
		}
		a.Detail = "AccountService not readable: " + shortErr(err)
		return a, nil
	}
	col, err := r.c.Get(ctx, as.Ref("Accounts"))
	if err != nil {
		if errors.Is(err, redfish.ErrUnreachable) {
			return nil, err
		}
		a.Detail = "accounts not readable: " + shortErr(err)
		return a, nil
	}
	// Exact match wins; otherwise a case-insensitive one (iDRAC and iLO
	// treat user names case-insensitively, most Linux BMCs do not).
	var acct, fold redfish.Object
	for _, ref := range col.Refs("Members") {
		o, err := r.c.Get(ctx, ref)
		if err != nil {
			if errors.Is(err, redfish.ErrUnreachable) {
				return nil, err
			}
			continue // accounts other than our own may be hidden
		}
		if o.Str("UserName") == user {
			acct = o
			break
		}
		if fold == nil && strings.EqualFold(o.Str("UserName"), user) {
			fold = o
		}
	}
	if acct == nil {
		acct = fold
	}
	if acct == nil {
		a.Detail = "account " + user + " is not visible in AccountService"
		return a, nil
	}
	a.Source = acct.ID()
	if role := acct.Str("RoleId"); role != "" {
		a.Role = role
		if privs := r.rolePrivileges(ctx, as, acct, role); privs != nil {
			a.Privileges = privs
			a.CanControl = boolp(canControl(privs))
		} else if p, ok := redfishStdRoles[role]; ok {
			a.Privileges = p
			a.CanControl = boolp(canControl(p))
		}
		return a, nil
	}
	// HPE iLO (and iLO 4 under Oem.Hp) publish boolean privileges instead of a RoleId.
	for _, k := range []string{"Oem.Hpe.Privileges", "Oem.Hp.Privileges"} {
		p := acct.Obj(k)
		if len(p) == 0 {
			continue
		}
		var on []string
		for name, v := range p {
			if b, _ := v.(bool); b {
				on = append(on, name)
			}
		}
		sort.Strings(on)
		has := func(n string) bool { b, _ := p[n].(bool); return b }
		ctl := has("VirtualPowerAndResetPriv") || has("iLOConfigPriv") || has("UserConfigPriv") || has("HostBIOSConfigPriv") || has("VirtualMediaPriv")
		switch {
		case has("UserConfigPriv") && has("iLOConfigPriv"):
			a.Role = "Administrator"
		case ctl:
			a.Role = "Operator"
		default:
			a.Role = "ReadOnly"
		}
		a.Privileges = on
		a.CanControl = boolp(ctl)
		a.Source += " (" + k + ")"
		return a, nil
	}
	a.Detail = "account has no RoleId"
	return a, nil
}

var redfishStdRoles = map[string][]string{
	"Administrator": {"Login", "ConfigureManager", "ConfigureUsers", "ConfigureSelf", "ConfigureComponents"},
	"Operator":      {"Login", "ConfigureSelf", "ConfigureComponents"},
	"ReadOnly":      {"Login", "ConfigureSelf"},
}

func (r *Redfish) rolePrivileges(ctx context.Context, as, acct redfish.Object, role string) []string {
	ref := acct.Ref("Links.Role")
	if ref == "" && as.Ref("Roles") != "" {
		ref = strings.TrimSuffix(as.Ref("Roles"), "/") + "/" + role
	}
	if ref == "" {
		return nil
	}
	o, err := r.c.Get(ctx, ref)
	if err != nil {
		return nil
	}
	privs := append(o.Strings("AssignedPrivileges"), o.Strings("OemPrivileges")...)
	if privs == nil {
		privs = []string{}
	}
	return privs
}

// ipmiRole names an IPMI privilege level.
func ipmiRole(l types.PrivilegeLevel) string {
	switch l {
	case types.PrivilegeLevelCallback:
		return "Callback"
	case types.PrivilegeLevelUser:
		return "User"
	case types.PrivilegeLevelOperator:
		return "Operator"
	case types.PrivilegeLevelAdministrator:
		return "Administrator"
	case types.PrivilegeLevelOEM:
		return "OEM"
	}
	return RoleUnknown
}

// effectiveIPMI combines the session's operating level with the user's
// channel limit (when readable): the lower one is what the account can do.
func effectiveIPMI(session, limit types.PrivilegeLevel) types.PrivilegeLevel {
	if limit >= types.PrivilegeLevelCallback && limit <= types.PrivilegeLevelOEM && limit < session {
		return limit
	}
	return session
}

// Account reports the session privilege (Get Session Info) and, where the
// BMC answers, the user's channel privilege limit (Get User Access).
func (b *IPMI) Account(ctx context.Context) (*Account, error) {
	c, err := b.conn(ctx)
	if err != nil {
		return nil, err
	}
	a := &Account{User: b.user, Role: RoleUnknown, Source: "IPMI Get Session Info"}
	si, err := c.GetCurrentSessionInfo(ctx)
	if err != nil {
		a.Detail = "Get Session Info: " + shortErr(err)
		return a, nil
	}
	eff := si.OperatingPrivilegeLevel
	a.Detail = "session privilege " + ipmiRole(eff)
	if ua, err := c.GetUserAccess(ctx, si.ChannelNumber, si.UserID); err == nil {
		eff = effectiveIPMI(eff, ua.MaxPrivLevel)
		a.Source += " + Get User Access"
		a.Detail += ", user limit " + ipmiRole(ua.MaxPrivLevel)
	}
	a.Role = ipmiRole(eff)
	if a.Role != RoleUnknown {
		a.CanControl = boolp(eff >= types.PrivilegeLevelOperator)
	}
	return a, nil
}
