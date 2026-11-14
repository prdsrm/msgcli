package mcpserver

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

func TestTokenValidation(t *testing.T) {
	jwks := newFakeJWKS(t)
	cfg := testConfig(t, nil)
	v := NewTokenValidator(cfg, jwks.server.URL, nil)

	id, exp, err := v.Validate(t.Context(), jwks.sign(t, validClaims(thomasOID, "Thomas@Example.com")))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if id.ObjectID != thomasOID || id.UPN != "thomas@example.com" || exp.Before(time.Now()) {
		t.Fatalf("identity = %+v exp=%v", id, exp)
	}

	// v1 issuer + App ID URI audience are accepted too.
	c := validClaims(thomasOID, "t@example.com")
	c["iss"] = "https://sts.windows.net/" + testTenant + "/"
	c["aud"] = "api://msgcli-mcp"
	if _, _, err := v.Validate(t.Context(), jwks.sign(t, c)); err != nil {
		t.Fatalf("v1 token rejected: %v", err)
	}

	otherKey := newFakeJWKS(t) // same kid, different key: bad signature

	cases := map[string]func(c jwt.MapClaims) string{
		"wrong audience": func(c jwt.MapClaims) string { c["aud"] = "api://other"; return jwks.sign(t, c) },
		"wrong issuer": func(c jwt.MapClaims) string {
			c["iss"] = "https://login.microsoftonline.com/99999999-2222-3333-4444-555555555555/v2.0"
			return jwks.sign(t, c)
		},
		"wrong tenant": func(c jwt.MapClaims) string {
			c["tid"] = "99999999-2222-3333-4444-555555555555"
			return jwks.sign(t, c)
		},
		"expired": func(c jwt.MapClaims) string {
			c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
			return jwks.sign(t, c)
		},
		"no exp":        func(c jwt.MapClaims) string { delete(c, "exp"); return jwks.sign(t, c) },
		"not yet valid": func(c jwt.MapClaims) string { c["nbf"] = time.Now().Add(time.Hour).Unix(); return jwks.sign(t, c) },
		"missing scope": func(c jwt.MapClaims) string { c["scp"] = "User.Read"; return jwks.sign(t, c) },
		"app-only token": func(c jwt.MapClaims) string {
			delete(c, "scp")
			c["roles"] = []string{"Mail.Read"}
			c["idtyp"] = "app"
			return jwks.sign(t, c)
		},
		"no oid":        func(c jwt.MapClaims) string { delete(c, "oid"); return jwks.sign(t, c) },
		"bad signature": func(c jwt.MapClaims) string { return otherKey.sign(t, c) },
		"unknown kid": func(c jwt.MapClaims) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
			tok.Header["kid"] = "nope"
			s, _ := tok.SignedString(jwks.key)
			return s
		},
		"HS256 alg confusion": func(c jwt.MapClaims) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
			tok.Header["kid"] = jwks.kid
			s, _ := tok.SignedString([]byte("secret"))
			return s
		},
		"alg none": func(c jwt.MapClaims) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodNone, c)
			s, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
			return s
		},
		"garbage": func(jwt.MapClaims) string { return "not.a.jwt" },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := v.Validate(t.Context(), mk(validClaims(thomasOID, "t@example.com")))
			if err == nil {
				t.Fatal("token accepted")
			}
			if !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("error must wrap ErrInvalidToken (401): %v", err)
			}
		})
	}
}

func TestJWKSRefreshIsRateLimited(t *testing.T) {
	jwks := newFakeJWKS(t)
	v := NewTokenValidator(testConfig(t, nil), jwks.server.URL, nil)
	for i := 0; i < 5; i++ {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims(thomasOID, "t@example.com"))
		tok.Header["kid"] = "unknown"
		s, _ := tok.SignedString(jwks.key)
		_, _, _ = v.Validate(t.Context(), s)
	}
	if jwks.hits != 1 {
		t.Fatalf("JWKS fetched %d times, want 1", jwks.hits)
	}
}
