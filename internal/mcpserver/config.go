// Package mcpserver exposes a read-only subset of msgcli as a remote MCP
// server (Streamable HTTP), protected by Microsoft Entra ID.
//
// The server is an OAuth 2.0 protected resource (RFC 9728). Entra ID is the
// authorization server: MCP clients sign the user in with Microsoft, get an
// Entra access token for this API, and send it as a bearer token. The server
// validates the token (signature via the tenant JWKS, issuer, audience,
// expiry, scope), then enforces an allowlist of users and a per-user mailbox
// policy. Mail is read with msgcli's app-only Graph credentials (AZURE_*).
package mcpserver

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Environment variables read by ConfigFromEnv. None of them is ever logged.
const (
	EnvListenAddr      = "MCP_LISTEN_ADDR"     // default ":8080"
	EnvPublicURL       = "MCP_PUBLIC_URL"      // e.g. https://mail-mcp.example.com (no trailing path)
	EnvTenantID        = "MCP_ENTRA_TENANT_ID" // defaults to AZURE_TENANT_ID
	EnvAudiences       = "MCP_ENTRA_AUDIENCES" // comma list: API app client ID and/or App ID URI
	EnvRequiredScope   = "MCP_ENTRA_SCOPE"     // short scope name in `scp`, default "Mail.Read.MCP"
	EnvScopeURI        = "MCP_ENTRA_SCOPE_URI" // full scope advertised to clients, e.g. api://<app-id>/Mail.Read.MCP
	EnvAllowedUsers    = "MCP_ALLOWED_USERS"   // comma list of Entra object IDs (oid) and/or UPNs
	EnvMailboxGrants   = "MCP_MAILBOX_GRANTS"  // optional extra mailboxes: "user=mbx1|mbx2;user2=mbx3"
	EnvAuthMode        = "MCP_AUTH_MODE"       // "proxy" (default) or "direct"
	EnvOAuthClientID   = "MCP_OAUTH_CLIENT_ID" // public client ID handed out by /oauth/register (proxy mode)
	EnvRedirectAllowed = "MCP_OAUTH_REDIRECTS" // optional comma list of allowed redirect URI prefixes (proxy mode)
)

// AuthMode selects how MCP clients reach Entra.
type AuthMode string

const (
	// AuthModeProxy advertises this server as the authorization server and
	// forwards /oauth/authorize and /oauth/token to Entra, smoothing over
	// Entra's gaps for MCP clients (no PKCE advertisement in its metadata,
	// no RFC 8707 `resource` parameter, no dynamic client registration).
	// Tokens are still issued and signed by Entra only.
	AuthModeProxy AuthMode = "proxy"
	// AuthModeDirect advertises Entra itself as the authorization server.
	AuthModeDirect AuthMode = "direct"
)

// Config is the server configuration.
type Config struct {
	ListenAddr    string
	PublicURL     string // base URL, no trailing slash
	TenantID      string
	Audiences     []string
	RequiredScope string
	ScopeURI      string
	AllowedUsers  []string // lower-cased oids or UPNs
	// Grants maps an allowlisted identity (oid or UPN, lower-cased) to extra
	// mailboxes it may read besides its own.
	Grants      map[string][]string
	AuthMode    AuthMode
	ClientID    string
	RedirectsOK []string
}

var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ConfigFromEnv reads and validates the configuration.
func ConfigFromEnv() (*Config, error) {
	return configFrom(os.Getenv)
}

func configFrom(getenv func(string) string) (*Config, error) {
	c := &Config{
		ListenAddr:    strings.TrimSpace(getenv(EnvListenAddr)),
		PublicURL:     strings.TrimRight(strings.TrimSpace(getenv(EnvPublicURL)), "/"),
		TenantID:      strings.TrimSpace(getenv(EnvTenantID)),
		RequiredScope: strings.TrimSpace(getenv(EnvRequiredScope)),
		ScopeURI:      strings.TrimSpace(getenv(EnvScopeURI)),
		AuthMode:      AuthMode(strings.ToLower(strings.TrimSpace(getenv(EnvAuthMode)))),
		ClientID:      strings.TrimSpace(getenv(EnvOAuthClientID)),
		Grants:        map[string][]string{},
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if c.TenantID == "" {
		c.TenantID = strings.TrimSpace(getenv("AZURE_TENANT_ID"))
	}
	if c.RequiredScope == "" {
		c.RequiredScope = "Mail.Read.MCP"
	}
	if c.AuthMode == "" {
		c.AuthMode = AuthModeProxy
	}
	c.Audiences = splitList(getenv(EnvAudiences), false)
	c.AllowedUsers = splitList(getenv(EnvAllowedUsers), true)
	c.RedirectsOK = splitList(getenv(EnvRedirectAllowed), false)

	var errs []string
	if c.PublicURL == "" {
		errs = append(errs, EnvPublicURL+" is required")
	} else if u, err := url.Parse(c.PublicURL); err != nil || u.Host == "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && isLocalHost(u.Hostname()))) {
		errs = append(errs, EnvPublicURL+" must be an https URL (http only for localhost)")
	}
	if !guidRe.MatchString(c.TenantID) {
		// Same rule as the leasing app (ADR-0004): a tenant GUID, never
		// "common" or "organizations".
		errs = append(errs, EnvTenantID+" (or AZURE_TENANT_ID) must be the tenant GUID")
	}
	if len(c.Audiences) == 0 {
		errs = append(errs, EnvAudiences+" is required (API app client ID and/or App ID URI)")
	}
	if len(c.AllowedUsers) == 0 {
		errs = append(errs, EnvAllowedUsers+" is required (fail closed: nobody is allowed by default)")
	}
	if c.ScopeURI == "" && len(c.Audiences) > 0 {
		aud := c.Audiences[0]
		if guidRe.MatchString(aud) {
			aud = "api://" + aud
		}
		c.ScopeURI = aud + "/" + c.RequiredScope
	}
	switch c.AuthMode {
	case AuthModeProxy, AuthModeDirect:
	default:
		errs = append(errs, EnvAuthMode+` must be "proxy" or "direct"`)
	}
	grants, err := parseGrants(getenv(EnvMailboxGrants))
	if err != nil {
		errs = append(errs, err.Error())
	}
	for user := range grants {
		if !contains(c.AllowedUsers, user) {
			errs = append(errs, fmt.Sprintf("%s: %q is not in %s", EnvMailboxGrants, user, EnvAllowedUsers))
		}
	}
	c.Grants = grants
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid MCP server configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return c, nil
}

// ResourceURL is the protected resource identifier (the MCP endpoint).
func (c *Config) ResourceURL() string { return c.PublicURL + "/mcp" }

// EntraIssuer is the Entra v2.0 issuer for the tenant.
func (c *Config) EntraIssuer() string {
	return "https://login.microsoftonline.com/" + c.TenantID + "/v2.0"
}

func parseGrants(s string) (map[string][]string, error) {
	out := map[string][]string{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		user, mbxs, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(user) == "" {
			return nil, fmt.Errorf("%s: entry %q must be user=mailbox1|mailbox2", EnvMailboxGrants, part)
		}
		var list []string
		for _, m := range strings.Split(mbxs, "|") {
			m = strings.ToLower(strings.TrimSpace(m))
			if m == "" {
				continue
			}
			if !strings.Contains(m, "@") {
				return nil, fmt.Errorf("%s: %q is not a mailbox address", EnvMailboxGrants, m)
			}
			list = append(list, m)
		}
		key := strings.ToLower(strings.TrimSpace(user))
		out[key] = append(out[key], list...)
	}
	return out, nil
}

func splitList(s string, lower bool) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if lower {
			v = strings.ToLower(v)
		}
		out = append(out, v)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func isLocalHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
