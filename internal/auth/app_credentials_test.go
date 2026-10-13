package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func clearAppEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvTenantID, "")
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")
	t.Setenv(EnvMailbox, "")
}

func setAppEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvTenantID, "tenant-123")
	t.Setenv(EnvClientID, "client-456")
	t.Setenv(EnvClientSecret, "secret-789")
}

func TestAppCredentialsFromEnv(t *testing.T) {
	t.Run("none set returns nil", func(t *testing.T) {
		clearAppEnv(t)
		creds, err := AppCredentialsFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if creds != nil {
			t.Fatalf("expected nil credentials, got %+v", creds)
		}
	})

	t.Run("all set returns credentials", func(t *testing.T) {
		clearAppEnv(t)
		setAppEnv(t)
		creds, err := AppCredentialsFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if creds == nil {
			t.Fatal("expected credentials, got nil")
		}
		if creds.TenantID != "tenant-123" || creds.ClientID != "client-456" || creds.ClientSecret != "secret-789" {
			t.Fatalf("unexpected credentials: %+v", creds)
		}
	})

	t.Run("partial set returns error", func(t *testing.T) {
		clearAppEnv(t)
		t.Setenv(EnvClientID, "client-456")
		_, err := AppCredentialsFromEnv()
		if err == nil {
			t.Fatal("expected error for partial credentials")
		}
		if !strings.Contains(err.Error(), EnvTenantID) || !strings.Contains(err.Error(), EnvClientSecret) {
			t.Fatalf("error should name the missing variables, got: %v", err)
		}
	})
}

func TestAppAuthEnabled(t *testing.T) {
	clearAppEnv(t)
	if AppAuthEnabled() {
		t.Fatal("expected app auth disabled with no env vars")
	}

	// Even a partial configuration routes into app mode so the user gets a
	// clear error instead of a silent fallback to keyring accounts.
	t.Setenv(EnvTenantID, "tenant-123")
	if !AppAuthEnabled() {
		t.Fatal("expected app auth enabled with partial env vars")
	}

	setAppEnv(t)
	if !AppAuthEnabled() {
		t.Fatal("expected app auth enabled with all env vars")
	}
}

func TestResolveAccountAppMode(t *testing.T) {
	clearAppEnv(t)
	setAppEnv(t)

	t.Run("flag wins", func(t *testing.T) {
		t.Setenv(EnvMailbox, "default@example.com")
		account, err := ResolveAccount("flag@example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if account != "flag@example.com" {
			t.Fatalf("expected flag@example.com, got %q", account)
		}
	})

	t.Run("env fallback", func(t *testing.T) {
		t.Setenv(EnvMailbox, "default@example.com")
		account, err := ResolveAccount("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if account != "default@example.com" {
			t.Fatalf("expected default@example.com, got %q", account)
		}
	})

	t.Run("neither errors", func(t *testing.T) {
		t.Setenv(EnvMailbox, "")
		_, err := ResolveAccount("")
		if err == nil {
			t.Fatal("expected error when no mailbox is specified in app mode")
		}
	})
}

func TestAppCredentialsGetToken(t *testing.T) {
	clearAppEnv(t)
	resetAppTokenCache()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if err := r.ParseForm(); err != nil {
			t.Fatalf("failed to parse form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", got)
		}
		if got := r.Form.Get("scope"); got != "https://graph.microsoft.com/.default" {
			t.Errorf("scope = %q, want graph .default", got)
		}
		if got := r.Form.Get("client_id"); got != "client-456" {
			t.Errorf("client_id = %q, want client-456", got)
		}
		if got := r.Form.Get("client_secret"); got != "secret-789" {
			t.Errorf("client_secret = %q, want secret-789", got)
		}
		if !strings.Contains(r.URL.Path, "tenant-123") {
			t.Errorf("token URL path %q should contain the tenant ID", r.URL.Path)
		}
		json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "app-token",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}))
	defer server.Close()

	oldURL := appTokenURL
	appTokenURL = server.URL + "/%s/oauth2/v2.0/token"
	defer func() { appTokenURL = oldURL; resetAppTokenCache() }()

	creds := &AppCredentials{TenantID: "tenant-123", ClientID: "client-456", ClientSecret: "secret-789"}

	token, err := creds.GetToken(t.Context())
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if token != "app-token" {
		t.Fatalf("token = %q, want app-token", token)
	}

	// Second call must be served from the in-process cache.
	token, err = creds.GetToken(t.Context())
	if err != nil {
		t.Fatalf("cached GetToken failed: %v", err)
	}
	if token != "app-token" {
		t.Fatalf("cached token = %q, want app-token", token)
	}
	if requests != 1 {
		t.Fatalf("expected 1 token request, got %d", requests)
	}
}

func TestAppCredentialsGetTokenError(t *testing.T) {
	resetAppTokenCache()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(TokenResponse{
			Error:     "invalid_client",
			ErrorDesc: "AADSTS7000215: Invalid client secret provided.",
		})
	}))
	defer server.Close()

	oldURL := appTokenURL
	appTokenURL = server.URL + "/%s/oauth2/v2.0/token"
	defer func() { appTokenURL = oldURL; resetAppTokenCache() }()

	creds := &AppCredentials{TenantID: "tenant-123", ClientID: "client-456", ClientSecret: "wrong"}

	_, err := creds.GetToken(t.Context())
	if err == nil {
		t.Fatal("expected error for invalid_client response")
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("error should include the OAuth error code, got: %v", err)
	}
}

func TestAppTokenCacheExpiry(t *testing.T) {
	resetAppTokenCache()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		// Token that expires immediately: must not be served from cache.
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "short-lived", ExpiresIn: 1})
	}))
	defer server.Close()

	oldURL := appTokenURL
	appTokenURL = server.URL + "/%s/oauth2/v2.0/token"
	defer func() { appTokenURL = oldURL; resetAppTokenCache() }()

	creds := &AppCredentials{TenantID: "t", ClientID: "c", ClientSecret: "s"}
	for range 2 {
		if _, err := creds.GetToken(t.Context()); err != nil {
			t.Fatalf("GetToken failed: %v", err)
		}
	}
	if requests != 2 {
		t.Fatalf("expected 2 token requests for expiring tokens, got %d", requests)
	}
}
