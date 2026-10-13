package bmc

import (
	"strings"
	"testing"

	"github.com/Blackman99/rackhands/internal/redfish"
	"github.com/Blackman99/rackhands/internal/testbmc"
	"github.com/bougou/go-ipmi/pkg/types"
)

func backendAs(t *testing.T, vendor, user string) (*Redfish, *testbmc.Server) {
	t.Helper()
	s := testbmc.Start(t, vendor)
	c, err := redfish.New(redfish.Options{BaseURL: s.URL, Username: user, Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	return NewRedfish(c, vendor+"-box", ""), s
}

func TestRedfishAccountRoles(t *testing.T) {
	cases := []struct {
		vendor, user, role string
		control            *bool
	}{
		{"dell", "root", "Administrator", boolp(true)},
		{"dell", "AGENT", "ReadOnly", boolp(false)}, // user names match case-insensitively
		{"hpe", "Administrator", "Administrator", boolp(true)},
		{"hpe", "monitor", "ReadOnly", boolp(false)},
		{"hpe", "ops", "Operator", boolp(true)}, // VirtualPowerAndResetPriv only
		{"lenovo", "admin", "ReadOnly", boolp(false)},
		{"lenovo", "fanwatch", "CustomRole1", boolp(true)}, // custom role with ConfigureComponents
		{"supermicro", "admin", "Operator", boolp(true)},
		{"supermicro", "nobody", RoleUnknown, nil},
	}
	for _, c := range cases {
		t.Run(c.vendor+"/"+c.user, func(t *testing.T) {
			b, s := backendAs(t, c.vendor, c.user)
			a, err := b.Account(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if a.Role != c.role {
				t.Fatalf("role = %q, want %q (%+v)", a.Role, c.role, a)
			}
			if (a.CanControl == nil) != (c.control == nil) || (a.CanControl != nil && *a.CanControl != *c.control) {
				t.Fatalf("can_control = %v, want %v", a.CanControl, c.control)
			}
			if len(s.Writes) != 0 {
				t.Fatalf("account check must be read-only, got writes %+v", s.Writes)
			}
		})
	}
}

func TestAccountWarning(t *testing.T) {
	admin := &Account{Role: "Administrator", CanControl: boolp(true)}
	admin.Check(nil)
	if !strings.Contains(admin.Warning, "read-only BMC account") {
		t.Fatalf("no warning for admin on a read-only target: %+v", admin)
	}
	allowed := &Account{Role: "Administrator", CanControl: boolp(true)}
	allowed.Check([]string{"power"})
	ro := &Account{Role: "ReadOnly", CanControl: boolp(false)}
	ro.Check(nil)
	unknown := &Account{Role: RoleUnknown}
	unknown.Check(nil)
	for _, a := range []*Account{allowed, ro, unknown} {
		if a.Warning != "" {
			t.Fatalf("unexpected warning %+v", a)
		}
	}
}

func TestInventoryWithAccount(t *testing.T) {
	b, _ := backendAs(t, "supermicro", "admin")
	inv, via, err := InventoryWithAccount(ctx, &Conn{Name: "x11", Primary: b})
	if err != nil || via.Protocol != "redfish" {
		t.Fatal(err, via)
	}
	if inv.Account == nil || inv.Account.Role != "Operator" || inv.Account.Warning == "" {
		t.Fatalf("got %+v", inv.Account)
	}
	if !strings.HasPrefix(inv.Notes[len(inv.Notes)-1], "WARNING: ") {
		t.Fatalf("notes %v", inv.Notes)
	}

	b2, _ := backendAs(t, "dell", "admin")
	inv, _, err = InventoryWithAccount(ctx, &Conn{Name: "r750", Primary: b2, Allow: []string{"power"}})
	if err != nil || inv.Account.Role != "Administrator" || inv.Account.Warning != "" {
		t.Fatalf("got %+v %v", inv.Account, err)
	}
}

func TestEffectiveIPMI(t *testing.T) {
	if got := effectiveIPMI(types.PrivilegeLevelAdministrator, types.PrivilegeLevelUser); got != types.PrivilegeLevelUser {
		t.Fatalf("user limit must cap the session: %v", got)
	}
	if got := effectiveIPMI(types.PrivilegeLevelOperator, 0x0f); got != types.PrivilegeLevelOperator {
		t.Fatalf("NO ACCESS / garbage limit must be ignored: %v", got)
	}
	if ipmiRole(types.PrivilegeLevelUser) != "User" || ipmiRole(types.PrivilegeLevelOEM) != "OEM" {
		t.Fatal("role names")
	}
	if !roleRejected(types.NewRmcpStatusError(types.RmcpStatusCodeInvalidRole)) || roleRejected(types.NewRmcpStatusError(types.RmcpStatusCodeNoCipherSuiteMatch)) {
		t.Fatal("roleRejected")
	}
}
