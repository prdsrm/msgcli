package mcpserver

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Identity is the signed-in caller, taken from a validated Entra token.
type Identity struct {
	ObjectID string // oid: immutable, preferred for the allowlist
	UPN      string // upn, or preferred_username when upn is absent (lower-cased)
	Name     string
	Scopes   []string
}

// Claims are the Entra access-token claims the server relies on.
type entraClaims struct {
	jwt.RegisteredClaims
	TenantID          string `json:"tid"`
	ObjectID          string `json:"oid"`
	UPN               string `json:"upn"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	Scope             string `json:"scp"`
	IDType            string `json:"idtyp"`
	Version           string `json:"ver"`
}

// TokenValidator validates Entra ID access tokens issued for this API.
type TokenValidator struct {
	tenantID  string
	issuers   []string
	audiences []string
	scope     string
	keys      *jwksCache
	leeway    time.Duration
	now       func() time.Time
}

// NewTokenValidator builds a validator for the tenant. jwksURL may be empty
// to use the tenant's public Entra keys.
func NewTokenValidator(cfg *Config, jwksURL string, httpClient *http.Client) *TokenValidator {
	if jwksURL == "" {
		jwksURL = "https://login.microsoftonline.com/" + cfg.TenantID + "/discovery/v2.0/keys"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &TokenValidator{
		tenantID: cfg.TenantID,
		// v2.0 tokens (accessTokenAcceptedVersion = 2) and v1.0 tokens.
		issuers: []string{
			"https://login.microsoftonline.com/" + cfg.TenantID + "/v2.0",
			"https://sts.windows.net/" + cfg.TenantID + "/",
		},
		audiences: cfg.Audiences,
		scope:     cfg.RequiredScope,
		keys:      &jwksCache{url: jwksURL, client: httpClient, minRefresh: time.Minute},
		leeway:    2 * time.Minute,
		now:       time.Now,
	}
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", auth.ErrInvalidToken, fmt.Sprintf(format, a...))
}

// Validate checks signature, issuer, audience, tenant, lifetime and scope.
// Every failure wraps auth.ErrInvalidToken (HTTP 401).
func (v *TokenValidator) Validate(ctx context.Context, raw string) (*Identity, time.Time, error) {
	var claims entraClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithLeeway(v.leeway),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	)
	_, err := parser.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token has no kid")
		}
		return v.keys.key(ctx, kid)
	})
	if err != nil {
		return nil, time.Time{}, invalid("%v", err)
	}
	if !contains(v.issuers, claims.Issuer) {
		return nil, time.Time{}, invalid("unexpected issuer")
	}
	if !strings.EqualFold(claims.TenantID, v.tenantID) {
		return nil, time.Time{}, invalid("unexpected tenant")
	}
	audOK := false
	for _, a := range claims.Audience {
		if contains(v.audiences, a) {
			audOK = true
		}
	}
	if !audOK {
		return nil, time.Time{}, invalid("unexpected audience")
	}
	// Delegated (user) tokens only: app-only tokens carry `roles`, not `scp`,
	// and would let any app with a secret in the tenant call the server.
	if claims.IDType == "app" || claims.Scope == "" {
		return nil, time.Time{}, invalid("a delegated user token is required")
	}
	scopes := strings.Fields(claims.Scope)
	if !contains(scopes, v.scope) {
		return nil, time.Time{}, invalid("missing scope %s", v.scope)
	}
	if claims.ObjectID == "" {
		return nil, time.Time{}, invalid("token has no oid")
	}
	upn := claims.UPN
	if upn == "" {
		upn = claims.PreferredUsername
	}
	id := &Identity{
		ObjectID: strings.ToLower(claims.ObjectID),
		UPN:      strings.ToLower(upn),
		Name:     claims.Name,
		Scopes:   scopes,
	}
	return id, claims.ExpiresAt.Time, nil
}

// Verifier adapts the validator to the MCP SDK's bearer middleware.
func (v *TokenValidator) Verifier() auth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		id, exp, err := v.Validate(ctx, token)
		if err != nil {
			return nil, err
		}
		return &auth.TokenInfo{
			Scopes:     id.Scopes,
			Expiration: exp,
			// The SDK binds MCP sessions to this ID (anti session hijack).
			UserID: id.ObjectID,
			Extra:  map[string]any{identityKey: id},
		}, nil
	}
}

const identityKey = "msgcli.identity"

// IdentityFrom returns the caller identity stored by Verifier.
func IdentityFrom(ti *auth.TokenInfo) *Identity {
	if ti == nil || ti.Extra == nil {
		return nil
	}
	id, _ := ti.Extra[identityKey].(*Identity)
	return id
}

// jwksCache fetches and caches RSA signing keys, refreshing on an unknown kid
// at most once per minRefresh (key rotation without hammering Entra).
type jwksCache struct {
	url        string
	client     *http.Client
	minRefresh time.Duration

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func (c *jwksCache) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	if !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.minRefresh {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func (c *jwksCache) refresh(ctx context.Context) error {
	c.fetchedAt = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching JWKS: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Use string `json:"use"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("parsing JWKS: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	c.keys = keys
	return nil
}
