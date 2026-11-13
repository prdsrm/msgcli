package mcpserver

import (
	"errors"
	"fmt"
	"strings"
)

// ErrForbidden is returned when the caller may not use the server or read
// the requested mailbox.
var ErrForbidden = errors.New("forbidden")

// ErrInvalidMailbox is returned for a malformed mailbox address.
var ErrInvalidMailbox = errors.New("invalid mailbox")

// Policy decides who may use the server and which mailboxes they may read.
// Default: allowlisted users only, each limited to their own mailbox.
// A grant of "*" lets a user read any mailbox (within MailboxDomains).
type Policy struct {
	allowed []string
	grants  map[string][]string
	domains []string
}

// NewPolicy builds the policy from the configuration.
func NewPolicy(cfg *Config) *Policy {
	return &Policy{allowed: cfg.AllowedUsers, grants: cfg.Grants, domains: cfg.MailboxDomains}
}

// AllMailboxAccess reports whether the identity may read every mailbox.
func (p *Policy) AllMailboxAccess(id *Identity) bool {
	if !p.Allowed(id) {
		return false
	}
	for _, key := range []string{id.ObjectID, id.UPN} {
		if key != "" && contains(p.grants[key], AllMailboxes) {
			return true
		}
	}
	return false
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

// Mailboxes lists the mailboxes the identity may read: its own first, then
// explicit grants; "*" in the list means every mailbox.
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
		requested = id.UPN
	}
	// Always a well-formed address: it becomes a Graph path segment.
	if !mailboxRe.MatchString(requested) {
		return "", fmt.Errorf("%w: %q is not a mailbox address", ErrInvalidMailbox, requested)
	}
	if !mailboxInDomains(requested, p.domains) {
		return "", fmt.Errorf("%w: %s is outside the allowed domains", ErrForbidden, requested)
	}
	if requested == id.UPN || contains(mbxs, requested) || p.AllMailboxAccess(id) {
		return requested, nil
	}
	return "", ErrForbidden
}
