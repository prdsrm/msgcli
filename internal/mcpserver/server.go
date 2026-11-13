package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// Version is reported to MCP clients.
var Version = "dev"

// Server bundles the HTTP handler and its dependencies.
type Server struct {
	cfg       *Config
	validator *TokenValidator
	policy    *Policy
	tools     *Tools
	audit     *slog.Logger
	// entraBase is https://login.microsoftonline.com (overridable in tests).
	entraBase string
	http      *http.Client
}

// Options are optional dependencies (tests inject fakes here).
type Options struct {
	JWKSURL    string
	EntraBase  string
	HTTPClient *http.Client
	Audit      *slog.Logger
}

// New builds the server.
func New(cfg *Config, readers ReaderFactory, opts *Options) *Server {
	if opts == nil {
		opts = &Options{}
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	audit := opts.Audit
	if audit == nil {
		audit = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	entra := opts.EntraBase
	if entra == "" {
		entra = "https://login.microsoftonline.com"
	}
	policy := NewPolicy(cfg)
	return &Server{
		cfg:       cfg,
		validator: NewTokenValidator(cfg, opts.JWKSURL, hc),
		policy:    policy,
		tools:     &Tools{Policy: policy, Reader: readers, Audit: audit},
		audit:     audit,
		entraBase: strings.TrimRight(entra, "/"),
		http:      hc,
	}
}

// Validator exposes the token validator (tests).
func (s *Server) Validator() *TokenValidator { return s.validator }

func (s *Server) prmURL() string {
	return s.cfg.PublicURL + "/.well-known/oauth-protected-resource/mcp"
}

// Handler returns the HTTP handler:
//
//	/mcp                                     MCP Streamable HTTP (bearer token required)
//	/.well-known/oauth-protected-resource[/mcp]  RFC 9728 metadata
//	/.well-known/oauth-authorization-server  RFC 8414 metadata (proxy mode)
//	/oauth/authorize, /oauth/token, /oauth/register  Entra pass-through (proxy mode)
//	/healthz
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	prm := &oauthex.ProtectedResourceMetadata{
		Resource:               s.cfg.ResourceURL(),
		AuthorizationServers:   []string{s.authServerIssuer()},
		ScopesSupported:        []string{s.cfg.ScopeURI},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "msgcli mail (read-only)",
	}
	prmHandler := auth.ProtectedResourceMetadataHandler(prm)
	mux.Handle("/.well-known/oauth-protected-resource", prmHandler)
	mux.Handle("/.well-known/oauth-protected-resource/mcp", prmHandler)

	if s.cfg.AuthMode == AuthModeProxy {
		mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleASMetadata)
		mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", s.handleASMetadata)
		mux.HandleFunc("/oauth/authorize", s.handleAuthorize)
		mux.HandleFunc("/oauth/token", s.handleToken)
		mux.HandleFunc("/oauth/register", s.handleRegister)
	}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "msgcli", Version: Version}, &mcp.ServerOptions{
		Instructions: "Read-only access to your own Outlook mailbox. You cannot send, reply, forward, move or delete mail through this server.",
	})
	s.tools.Register(mcpServer)
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)

	bearer := auth.RequireBearerToken(s.validator.Verifier(), &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.prmURL(),
		ClockSkew:           2 * time.Minute,
	})
	mux.Handle("/mcp", bearer(s.requireAllowlisted(streamable)))
	return mux
}

// requireAllowlisted rejects signed-in users who are not on the allowlist
// with 403 before any MCP processing.
func (s *Server) requireAllowlisted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFrom(auth.TokenInfoFromContext(r.Context()))
		if !s.policy.Allowed(id) {
			attrs := []slog.Attr{slog.String("outcome", "denied"), slog.String("reason", "not_allowlisted")}
			if id != nil {
				attrs = append(attrs, slog.String("user_oid", id.ObjectID), slog.String("user_upn", id.UPN))
			}
			s.audit.LogAttrs(r.Context(), slog.LevelWarn, "access_denied", attrs...)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden","error_description":"this account is not allowed to use this server"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authServerIssuer() string {
	if s.cfg.AuthMode == AuthModeProxy {
		return s.cfg.PublicURL
	}
	return s.cfg.EntraIssuer()
}

func (s *Server) entraEndpoint(name string) string {
	return s.entraBase + "/" + url.PathEscape(s.cfg.TenantID) + "/oauth2/v2.0/" + name
}

// asMetadata is RFC 8414 metadata including the PKCE field Entra's own
// discovery document omits (MCP clients must refuse an AS without it).
type asMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	JWKSURI                           string   `json:"jwks_uri"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
}

func (s *Server) handleASMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	md := asMetadata{
		Issuer:                            s.cfg.PublicURL,
		AuthorizationEndpoint:             s.cfg.PublicURL + "/oauth/authorize",
		TokenEndpoint:                     s.cfg.PublicURL + "/oauth/token",
		JWKSURI:                           s.entraBase + "/" + url.PathEscape(s.cfg.TenantID) + "/discovery/v2.0/keys",
		ScopesSupported:                   []string{s.cfg.ScopeURI, "offline_access"},
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:     []string{"S256"},
	}
	if s.cfg.ClientID != "" {
		md.RegistrationEndpoint = s.cfg.PublicURL + "/oauth/register"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(md)
}

func oauthError(w http.ResponseWriter, code int, errCode, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": errCode, "error_description": desc})
}

func (s *Server) redirectAllowed(uri string) bool {
	if len(s.cfg.RedirectsOK) == 0 {
		return true // Entra still enforces its registered redirect URIs.
	}
	for _, p := range s.cfg.RedirectsOK {
		if strings.HasPrefix(uri, p) {
			return true
		}
	}
	return false
}

// handleAuthorize forwards the browser to Entra's authorize endpoint, with
// PKCE required, the `resource` parameter (unsupported by Entra v2) removed,
// and the API scope ensured.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	if q.Get("response_type") != "code" {
		oauthError(w, http.StatusBadRequest, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "PKCE with S256 is required")
		return
	}
	if s.cfg.ClientID != "" && q.Get("client_id") != s.cfg.ClientID {
		oauthError(w, http.StatusBadRequest, "invalid_client", "unknown client_id")
		return
	}
	if !s.redirectAllowed(q.Get("redirect_uri")) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri not allowed")
		return
	}
	q.Del("resource")
	q.Set("scope", s.normalizeScope(q.Get("scope")))
	http.Redirect(w, r, s.entraEndpoint("authorize")+"?"+q.Encode(), http.StatusFound)
}

// normalizeScope keeps only scopes Entra should see for this API, and always
// requests the API scope and offline_access (refresh tokens).
func (s *Server) normalizeScope(in string) string {
	out := []string{s.cfg.ScopeURI}
	for _, sc := range strings.Fields(in) {
		switch sc {
		case "openid", "profile", "email", "offline_access":
			if !contains(out, sc) {
				out = append(out, sc)
			}
		}
	}
	if !contains(out, "offline_access") {
		out = append(out, "offline_access")
	}
	return strings.Join(out, " ")
}

// handleToken forwards a token request to Entra (minus `resource`). The body
// holds codes and refresh tokens: it is never logged.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	form := r.PostForm
	switch form.Get("grant_type") {
	case "authorization_code", "refresh_token":
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
		return
	}
	if s.cfg.ClientID != "" && form.Get("client_id") != "" && form.Get("client_id") != s.cfg.ClientID {
		oauthError(w, http.StatusBadRequest, "invalid_client", "unknown client_id")
		return
	}
	form.Del("resource")
	if form.Get("scope") != "" || form.Get("grant_type") == "refresh_token" {
		form.Set("scope", s.normalizeScope(form.Get("scope")))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.entraEndpoint("token"), strings.NewReader(form.Encode()))
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not build request")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if a := r.Header.Get("Authorization"); a != "" { // confidential clients (client_secret_basic)
		req.Header.Set("Authorization", a)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		oauthError(w, http.StatusBadGateway, "temporarily_unavailable", "could not reach Microsoft Entra ID")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// handleRegister implements a "static" dynamic client registration (RFC
// 7591): Entra has no DCR, so every MCP client gets the one public client
// registered in Entra for this server. Entra still only accepts the redirect
// URIs registered on that app.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ClientID == "" {
		oauthError(w, http.StatusNotFound, "invalid_request", "registration is not enabled")
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "malformed JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !s.redirectAllowed(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri not allowed: "+u)
			return
		}
	}
	s.audit.LogAttrs(r.Context(), slog.LevelInfo, "client_registered",
		slog.String("client_name", req.ClientName), slog.Any("redirect_uris", req.RedirectURIs))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id":                  s.cfg.ClientID,
		"client_id_issued_at":        time.Now().Unix(),
		"redirect_uris":              req.RedirectURIs,
		"client_name":                req.ClientName,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// ListenAndServe runs the server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
