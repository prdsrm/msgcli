package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// Microsoft identity platform endpoints
	// Using "common" tenant for multi-tenant (personal + work/school)
	authorizeEndpoint = "https://login.microsoftonline.com/common/oauth2/v2.0/devicecode"
	tokenEndpoint     = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	graphEndpoint     = "https://graph.microsoft.com/v1.0"

	// Scopes for mail and calendar access
	defaultScopes = "offline_access User.Read Mail.ReadWrite Mail.Send Calendars.ReadWrite"
)

// DeviceCodeResponse is the initial response from the device code endpoint
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Message         string `json:"message"`
}

// TokenResponse is the response from the token endpoint
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token,omitempty"`
	Error        string `json:"error,omitempty"`
	ErrorDesc    string `json:"error_description,omitempty"`
}

// UserInfo holds basic user information from Graph API
type UserInfo struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	UserPrincipalName string `json:"userPrincipalName"`
	Mail              string `json:"mail"`
}

// StartDeviceCodeFlow initiates the device code authentication flow
func StartDeviceCodeFlow(ctx context.Context, clientID string) (*DeviceCodeResponse, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("scope", defaultScopes)

	req, err := http.NewRequestWithContext(ctx, "POST", authorizeEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dcr DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dcr); err != nil {
		return nil, err
	}

	return &dcr, nil
}

// PollForToken polls the token endpoint until the user authenticates
func PollForToken(ctx context.Context, clientID string, deviceCode string, interval int) (*TokenResponse, error) {
	if interval < 1 {
		interval = 5
	}

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	data.Set("device_code", deviceCode)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, "POST", tokenEndpoint, strings.NewReader(data.Encode()))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}

			var tr TokenResponse
			err = json.NewDecoder(resp.Body).Decode(&tr)
			resp.Body.Close()
			if err != nil {
				return nil, err
			}

			switch tr.Error {
			case "":
				// Success!
				return &tr, nil
			case "authorization_pending":
				// Keep polling
				continue
			case "slow_down":
				// Increase interval
				interval += 5
				ticker.Reset(time.Duration(interval) * time.Second)
				continue
			case "authorization_declined":
				return nil, errors.New("user declined authorization")
			case "expired_token":
				return nil, errors.New("device code expired - please try again")
			default:
				return nil, fmt.Errorf("authentication error: %s - %s", tr.Error, tr.ErrorDesc)
			}
		}
	}
}

// RefreshAccessToken uses a refresh token to get a new access token
func RefreshAccessToken(ctx context.Context, clientID, refreshToken string) (*TokenResponse, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("scope", defaultScopes)

	req, err := http.NewRequestWithContext(ctx, "POST", tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}

	if tr.Error != "" {
		return nil, fmt.Errorf("refresh failed: %s - %s", tr.Error, tr.ErrorDesc)
	}

	return &tr, nil
}

// GetUserInfo fetches the current user's info from Graph API
func GetUserInfo(ctx context.Context, accessToken string) (*UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", graphEndpoint+"/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get user info: %s", resp.Status)
	}

	var user UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, err
	}

	return &user, nil
}

// GetValidToken returns a valid access token for the given account, refreshing if needed.
// When app-only credentials are set in the environment (AZURE_TENANT_ID,
// AZURE_CLIENT_ID, AZURE_CLIENT_SECRET), they take precedence and the
// config/keyring are bypassed entirely.
func GetValidToken(ctx context.Context, alias string) (string, error) {
	if AppAuthEnabled() {
		creds, err := AppCredentialsFromEnv()
		if err != nil {
			return "", err
		}
		return creds.GetToken(ctx)
	}

	config, err := LoadConfig()
	if err != nil {
		return "", err
	}

	token, err := LoadToken(alias)
	if err != nil {
		return "", err
	}

	// Check if token is expired (with 5 minute buffer)
	if time.Now().Unix() > token.ExpiresAt-300 {
		// Token expired or expiring soon, refresh it
		tr, err := RefreshAccessToken(ctx, config.ClientID, token.RefreshToken)
		if err != nil {
			return "", fmt.Errorf("failed to refresh token: %w", err)
		}

		// Update stored token
		token.AccessToken = tr.AccessToken
		if tr.RefreshToken != "" {
			token.RefreshToken = tr.RefreshToken
		}
		token.ExpiresAt = time.Now().Unix() + int64(tr.ExpiresIn)

		if err := SaveToken(alias, token); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save refreshed token: %v\n", err)
		}
	}

	return token.AccessToken, nil
}

// ResolveAccount returns the account alias to use, resolving default if needed.
// In app-only mode the "account" is the target mailbox address (UPN), taken
// from --account or the MSGCLI_MAILBOX environment variable.
func ResolveAccount(accountFlag string) (string, error) {
	if AppAuthEnabled() {
		mailbox := cmp.Or(accountFlag, getEnv(EnvMailbox))
		if mailbox == "" {
			return "", errors.New("app-only auth requires a target mailbox - pass --account <email> or set " + EnvMailbox)
		}
		return mailbox, nil
	}

	if accountFlag != "" {
		return accountFlag, nil
	}
	return GetDefaultAccount()
}
