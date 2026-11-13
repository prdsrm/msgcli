package mcpserver

import (
	"errors"
	"testing"
)

func TestPolicy(t *testing.T) {
	cfg := testConfig(t, map[string]string{EnvMailboxGrants: thomasOID + "=contact@example.com"})
	p := NewPolicy(cfg)
	thomas := &Identity{ObjectID: thomasOID, UPN: "thomas@example.com"}
	hugo := &Identity{ObjectID: hugoOID, UPN: "hugo@example.com"} // allowlisted by UPN
	outsider := &Identity{ObjectID: outsiderOID, UPN: "romeo@example.com"}

	if !p.Allowed(thomas) || !p.Allowed(hugo) {
		t.Fatal("Thomas (by oid) and Hugo (by UPN) must be allowed")
	}
	if p.Allowed(outsider) || p.Allowed(nil) {
		t.Fatal("outsider and nil must be denied")
	}

	for _, tc := range []struct {
		name      string
		id        *Identity
		requested string
		want      string
		forbidden bool
	}{
		{"own mailbox by default", hugo, "", "hugo@example.com", false},
		{"own mailbox explicit, case-insensitive", hugo, "HUGO@example.com", "hugo@example.com", false},
		{"other user's mailbox denied", hugo, "thomas@example.com", "", true},
		{"granted shared mailbox", thomas, "contact@example.com", "contact@example.com", false},
		{"grant does not leak to others", hugo, "contact@example.com", "", true},
		{"outsider denied even own", outsider, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ResolveMailbox(tc.id, tc.requested)
			if tc.forbidden {
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("want ErrForbidden, got %q, %v", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
