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

func TestPolicyAllMailboxes(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		EnvAllowedUsers:   thomasOID + ",hugo@example.com," + hugoOID,
		EnvMailboxGrants:  thomasOID + "=*",
		EnvMailboxDomains: "example.com",
	})
	p := NewPolicy(cfg)
	thomas := &Identity{ObjectID: thomasOID, UPN: "thomas@example.com"}
	hugo := &Identity{ObjectID: hugoOID, UPN: "hugo@example.com"}
	outsider := &Identity{ObjectID: outsiderOID, UPN: "eve@example.com"}

	if !p.AllMailboxAccess(thomas) || p.AllMailboxAccess(hugo) || p.AllMailboxAccess(outsider) {
		t.Fatal("only Thomas has all-mailbox access")
	}
	for _, tc := range []struct {
		name      string
		id        *Identity
		requested string
		want      string
		err       error
	}{
		{"wildcard reads any mailbox in domain", thomas, "Anyone@Example.com", "anyone@example.com", nil},
		{"wildcard still defaults to own", thomas, "", "thomas@example.com", nil},
		{"wildcard limited to domains", thomas, "x@other.com", "", ErrForbidden},
		{"wildcard rejects malformed", thomas, "../x@example.com", "", ErrInvalidMailbox},
		{"wildcard rejects path chars", thomas, "a/b@example.com", "", ErrInvalidMailbox},
		{"wildcard rejects query chars", thomas, "a@example.com?$x", "", ErrInvalidMailbox},
		{"no wildcard: other mailbox denied", hugo, "anyone@example.com", "", ErrForbidden},
		{"outsider denied", outsider, "anyone@example.com", "", ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ResolveMailbox(tc.id, tc.requested)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("want %v, got %q, %v", tc.err, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
