package mcpserver

import (
	"errors"
	"strings"
)

// ErrForbidden is returned when the caller may not use the server or read
// the requested mailbox.
var ErrForbidden = errors.New("forbidden")

// Policy decides who may use the server and which mailboxes they may read.
// Default: allowlisted users only, each limited to their own mailbox.
type Policy struct {
	allowed []string
	grants  map[string][]string
}

// NewPolicy builds the policy from the configuration.
func NewPolicy(cfg *Config) *Policy {
	return &Policy{allowed: cfg.AllowedUsers, grants: cfg.Grants}
}

// Allowed reports whether the identity is on the allowlist, by object ID
// (preferred, immutable) or UPN.
func (p *Policy) Allowed(id *Identity) bool {
	if id == nil {
		return false
	}
	for _, a := range p.allowed {
		if a == id.ObjectID || (id.UPN != "" && a == id.UPN) {
			return true
		}
	}
	return false
}

// Mailboxes lists the mailboxes the identity may read: its own first.
func (p *Policy) Mailboxes(id *Identity) []string {
	if !p.Allowed(id) {
		return nil
	}
	var out []string
	if id.UPN != "" {
		out = append(out, id.UPN)
	}
	for _, key := range []string{id.ObjectID, id.UPN} {
		for _, m := range p.grants[key] {
			if !contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// ResolveMailbox returns the mailbox to read: the caller's own when
// requested is empty, otherwise requested if the policy grants it.
func (p *Policy) ResolveMailbox(id *Identity, requested string) (string, error) {
	mbxs := p.Mailboxes(id)
	if len(mbxs) == 0 {
		return "", ErrForbidden
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		if id.UPN == "" {
			return "", errors.New("token has no UPN; pass a mailbox explicitly")
		}
		return id.UPN, nil
	}
	if contains(mbxs, requested) {
		return requested, nil
	}
	return "", ErrForbidden
}
