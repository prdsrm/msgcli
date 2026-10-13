package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Environment variables for app-only (client credentials) authentication.
// When set, they take precedence over device-code accounts stored in the
// keyring — no `auth setup` or `auth add` is needed.
const (
	EnvTenantID     = "AZURE_TENANT_ID"
	EnvClientID     = "AZURE_CLIENT_ID"
	EnvClientSecret = "AZURE_CLIENT_SECRET"

	// EnvMailbox optionally sets the default target mailbox in app-only
	// mode; the --account flag overrides it per command.
	EnvMailbox = "MSGCLI_MAILBOX"
)

// appTokenURL is a fmt template taking the tenant ID; a var so tests can
// point it at a local server.
var appTokenURL = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"

// AppCredentials holds client-credentials (app-only) settings read from the
// environment.
type AppCredentials struct {
	TenantID     string
	ClientID     string
	ClientSecret string
}

// AppCredentialsFromEnv reads the AZURE_* variables. It returns (nil, nil)
// when none are set (delegated device-code mode applies) and an error naming
// the missing variables when only some are set.
func AppCredentialsFromEnv() (*AppCredentials, error) {
	creds := &AppCredentials{
		TenantID:     getEnv(EnvTenantID),
		ClientID:     getEnv(EnvClientID),
		ClientSecret: getEnv(EnvClientSecret),
	}

	if creds.TenantID == "" && creds.ClientID == "" && creds.ClientSecret == "" {
		return nil, nil
	}

	var missing []string
	for env, value := range map[string]string{
		EnvTenantID:     creds.TenantID,
		EnvClientID:     creds.ClientID,
		EnvClientSecret: creds.ClientSecret,
	} {
		if value == "" {
			missing = append(missing, env)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("incomplete app credentials in environment: %s not set", strings.Join(missing, ", "))
	}

	return creds, nil
}

// AppAuthEnabled reports whether app-only credentials are present in the
// environment. A partial configuration still counts so that authentication
// fails with a clear error instead of silently falling back to keyring
// accounts.
func AppAuthEnabled() bool {
	return getEnv(EnvTenantID) != "" || getEnv(EnvClientID) != "" || getEnv(EnvClientSecret) != ""
}

// appTokenCache holds the current app-only token for the life of the process
// so each Graph request doesn't trigger a token request.
var appTokenCache struct {
	mu        sync.Mutex
	key       string
	token     string
	expiresAt time.Time
}

func resetAppTokenCache() {
	appTokenCache.mu.Lock()
	defer appTokenCache.mu.Unlock()
	appTokenCache.key = ""
	appTokenCache.token = ""
	appTokenCache.expiresAt = time.Time{}
}

// GetToken returns an app-only Microsoft Graph access token via the OAuth2
// client credentials grant, cached in-process until shortly before expiry.
// Permissions come from the app registration's granted application
// permissions (scope is always graph.microsoft.com/.default).
func (c *AppCredentials) GetToken(ctx context.Context) (string, error) {
	appTokenCache.mu.Lock()
	defer appTokenCache.mu.Unlock()

	cacheKey := c.TenantID + "|" + c.ClientID
	if appTokenCache.key == cacheKey && appTokenCache.token != "" && time.Until(appTokenCache.expiresAt) > time.Minute {
		return appTokenCache.token, nil
	}

	data := url.Values{}
	data.Set("client_id", c.ClientID)
	data.Set("client_secret", c.ClientSecret)
	data.Set("grant_type", "client_credentials")
	data.Set("scope", "https://graph.microsoft.com/.default")

	endpoint := fmt.Sprintf(appTokenURL, url.PathEscape(c.TenantID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}

	if tr.Error != "" {
		return "", fmt.Errorf("client credentials authentication failed: %s - %s", tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return "", errors.New("token endpoint returned no access token")
	}

	appTokenCache.key = cacheKey
	appTokenCache.token = tr.AccessToken
	appTokenCache.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)

	return tr.AccessToken, nil
}
