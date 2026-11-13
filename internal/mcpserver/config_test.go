package mcpserver

import (
	"strings"
	"testing"
)

func TestConfigFailsClosed(t *testing.T) {
	_, err := configFrom(func(string) string { return "" })
	if err == nil {
		t.Fatal("empty config must be rejected")
	}
	for _, want := range []string{EnvPublicURL, EnvTenantID, EnvAudiences, EnvAllowedUsers} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestConfigRejectsCommonTenantAndHTTP(t *testing.T) {
	env := map[string]string{
		EnvPublicURL: "http://mail-mcp.example.com", EnvTenantID: "common",
		EnvAudiences: testAudience, EnvAllowedUsers: thomasOID,
	}
	_, err := configFrom(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "https") || !strings.Contains(err.Error(), "tenant GUID") {
		t.Fatalf("want https and tenant errors, got %v", err)
	}
}

func TestConfigDefaultsAndGrants(t *testing.T) {
	cfg := testConfig(t, map[string]string{EnvMailboxGrants: thomasOID + "=Contact@Example.com|ops@example.com"})
	if cfg.ScopeURI != "api://"+testAudience+"/Mail.Read.MCP" {
		t.Errorf("ScopeURI = %q", cfg.ScopeURI)
	}
	if cfg.AuthMode != AuthModeProxy || cfg.ListenAddr != ":8080" {
		t.Errorf("defaults: %q %q", cfg.AuthMode, cfg.ListenAddr)
	}
	if got := cfg.Grants[thomasOID]; len(got) != 2 || got[0] != "contact@example.com" {
		t.Errorf("grants = %v", got)
	}
	if cfg.ResourceURL() != "https://mail-mcp.example.com/mcp" {
		t.Errorf("resource = %q", cfg.ResourceURL())
	}
}

func TestConfigRejectsGrantForUnlistedUser(t *testing.T) {
	env := map[string]string{
		EnvPublicURL: "https://x.example.com", EnvTenantID: testTenant,
		EnvAudiences: testAudience, EnvAllowedUsers: thomasOID,
		EnvMailboxGrants: outsiderOID + "=contact@example.com",
	}
	if _, err := configFrom(func(k string) string { return env[k] }); err == nil {
		t.Fatal("grant for a user outside the allowlist must be rejected")
	}
}
