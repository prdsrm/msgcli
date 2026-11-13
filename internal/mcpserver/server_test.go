package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type harness struct {
	jwks  *fakeJWKS
	srv   *httptest.Server
	calls *[]string
	audit *bytes.Buffer
	mu    sync.Mutex
}

func newHarness(t *testing.T, cfgExtra map[string]string, opts *Options) *harness {
	t.Helper()
	h := &harness{jwks: newFakeJWKS(t), calls: &[]string{}, audit: &bytes.Buffer{}}
	cfg := testConfig(t, cfgExtra)
	if opts == nil {
		opts = &Options{}
	}
	opts.JWKSURL = h.jwks.server.URL
	opts.Audit = slog.New(slog.NewJSONHandler(h.audit, nil))
	s := New(cfg, func(mbx string) MailReader {
		h.mu.Lock()
		defer h.mu.Unlock()
		return fakeReader{mailbox: mbx, calls: h.calls}
	}, opts)
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) connect(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             h.srv.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerRT{token}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
}

func TestUnauthenticatedGets401WithResourceMetadata(t *testing.T) {
	h := newHarness(t, nil, nil)
	resp, err := http.Post(h.srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wa := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(wa, `resource_metadata="https://mail-mcp.example.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("WWW-Authenticate = %q", wa)
	}
}

func TestProtectedResourceMetadata(t *testing.T) {
	h := newHarness(t, nil, nil)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		resp, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var md map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&md)
		resp.Body.Close()
		if md["resource"] != "https://mail-mcp.example.com/mcp" {
			t.Errorf("%s resource = %v", path, md["resource"])
		}
		as, _ := md["authorization_servers"].([]any)
		if len(as) != 1 || as[0] != "https://mail-mcp.example.com" {
			t.Errorf("%s authorization_servers = %v", path, as)
		}
	}
}

func TestDirectModeAdvertisesEntra(t *testing.T) {
	h := newHarness(t, map[string]string{EnvAuthMode: "direct"}, nil)
	resp, _ := http.Get(h.srv.URL + "/.well-known/oauth-protected-resource")
	var md map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&md)
	resp.Body.Close()
	as, _ := md["authorization_servers"].([]any)
	if len(as) != 1 || as[0] != "https://login.microsoftonline.com/"+testTenant+"/v2.0" {
		t.Fatalf("authorization_servers = %v", as)
	}
	if r, _ := http.Get(h.srv.URL + "/oauth/authorize"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("proxy endpoints must be off in direct mode, got %d", r.StatusCode)
	}
}

func TestNonAllowlistedUserGets403(t *testing.T) {
	h := newHarness(t, nil, nil)
	tok := h.jwks.sign(t, validClaims(outsiderOID, "romeo@example.com"))
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(h.audit.String(), `"access_denied"`) || !strings.Contains(h.audit.String(), outsiderOID) {
		t.Fatalf("denial not audited: %s", h.audit.String())
	}
	if _, err := h.connect(t, tok); err == nil {
		t.Fatal("MCP connect must fail for a non-allowlisted user")
	}
}

func TestToolsAreReadOnly(t *testing.T) {
	h := newHarness(t, nil, nil)
	cs, err := h.connect(t, h.jwks.sign(t, validClaims(thomasOID, "thomas@example.com")))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is not marked read-only", tl.Name)
		}
		for _, bad := range []string{"send", "reply", "forward", "delete", "move", "draft", "update"} {
			if strings.Contains(tl.Name, bad) {
				t.Errorf("write tool exposed: %s", tl.Name)
			}
		}
	}
	want := "attachment_get,attachment_list,folders_list,mail_get,mail_list,whoami"
	if got := strings.Join(sortStrings(names), ","); got != want {
		t.Fatalf("tools = %s, want %s", got, want)
	}
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		return nil, true
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, false
}

func TestToolHandlers(t *testing.T) {
	h := newHarness(t, map[string]string{EnvMailboxGrants: thomasOID + "=contact@example.com"}, nil)
	hugo, err := h.connect(t, h.jwks.sign(t, validClaims(hugoOID, "Hugo@Example.com")))
	if err != nil {
		t.Fatal(err)
	}
	defer hugo.Close()

	out, isErr := call(t, hugo, "mail_list", map[string]any{"after": "2026-11-01"})
	if isErr || out["mailbox"] != "hugo@example.com" {
		t.Fatalf("mail_list: %v %v", out, isErr)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["from"] != "A <a@x.com>" {
		t.Fatalf("messages = %v", msgs)
	}
	if out, isErr = call(t, hugo, "mail_list", map[string]any{"query": "from:bob"}); isErr {
		t.Fatalf("search failed: %v", out)
	}
	if out, isErr = call(t, hugo, "mail_get", map[string]any{"id": "AAMkAD-x_y="}); isErr || out["body"] != "body text" {
		t.Fatalf("mail_get: %v", out)
	}
	if out, isErr = call(t, hugo, "attachment_list", map[string]any{"message_id": "m1"}); isErr {
		t.Fatalf("attachment_list: %v", out)
	}
	if out, isErr = call(t, hugo, "attachment_get", map[string]any{"message_id": "m1", "attachment_id": "a1"}); isErr || out["text"] != "hello" {
		t.Fatalf("attachment_get text: %v", out)
	}
	if out, isErr = call(t, hugo, "attachment_get", map[string]any{"message_id": "m1", "attachment_id": "bin"}); isErr || out["content_base64"] != "/wAB" {
		t.Fatalf("attachment_get binary: %v", out)
	}
	if _, isErr = call(t, hugo, "attachment_get", map[string]any{"message_id": "m1", "attachment_id": "big"}); !isErr {
		t.Fatal("oversized attachment must fail")
	}
	if out, isErr = call(t, hugo, "folders_list", nil); isErr {
		t.Fatalf("folders_list: %v", out)
	}
	if out, isErr = call(t, hugo, "whoami", nil); isErr || out["upn"] != "hugo@example.com" {
		t.Fatalf("whoami: %v", out)
	}

	// Mailbox isolation.
	callsBefore := len(*h.calls)
	if _, isErr = call(t, hugo, "mail_list", map[string]any{"mailbox": "thomas@example.com"}); !isErr {
		t.Fatal("Hugo must not read Thomas's mailbox")
	}
	if _, isErr = call(t, hugo, "mail_list", map[string]any{"mailbox": "contact@example.com"}); !isErr {
		t.Fatal("Thomas's grant must not apply to Hugo")
	}
	// Path injection through IDs.
	for _, bad := range []string{"../../users/thomas@example.com/messages/x", "m1?$select=body", "a/b", "a b", ""} {
		if _, isErr = call(t, hugo, "mail_get", map[string]any{"id": bad}); !isErr {
			t.Fatalf("id %q must be rejected", bad)
		}
	}
	if _, isErr = call(t, hugo, "mail_list", map[string]any{"folder": "inbox/../../x"}); !isErr {
		t.Fatal("bad folder must be rejected")
	}
	if _, isErr = call(t, hugo, "mail_list", map[string]any{"query": "x", "folder": "inbox"}); !isErr {
		t.Fatal("query+folder must be rejected")
	}
	if len(*h.calls) != callsBefore {
		t.Fatalf("Graph was called on a denied/invalid request: %v", (*h.calls)[callsBefore:])
	}
	for _, c := range *h.calls {
		if !strings.HasPrefix(c, "hugo@example.com:") {
			t.Fatalf("Graph call on another mailbox: %s", c)
		}
	}

	// Thomas may read the granted shared mailbox.
	thomas, err := h.connect(t, h.jwks.sign(t, validClaims(thomasOID, "thomas@example.com")))
	if err != nil {
		t.Fatal(err)
	}
	defer thomas.Close()
	if out, isErr = call(t, thomas, "mail_list", map[string]any{"mailbox": "contact@example.com"}); isErr || out["mailbox"] != "contact@example.com" {
		t.Fatalf("granted mailbox: %v", out)
	}

	// Audit log: one JSON line per tool call with user, tool and args.
	var sawGet, sawDenied bool
	for _, line := range strings.Split(strings.TrimSpace(h.audit.String()), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line is not JSON: %s", line)
		}
		if e["msg"] != "tool_call" {
			continue
		}
		if e["tool"] == "mail_get" && e["user_oid"] == hugoOID && e["outcome"] == "ok" {
			sawGet = true
			if strings.Contains(line, "body text") {
				t.Fatal("audit log must not contain message bodies")
			}
		}
		if e["tool"] == "mail_list" && e["outcome"] == "error" && strings.Contains(line, "forbidden") {
			sawDenied = true
		}
	}
	if !sawGet || !sawDenied {
		t.Fatalf("audit incomplete (get=%v denied=%v):\n%s", sawGet, sawDenied, h.audit.String())
	}
}

func TestOAuthProxy(t *testing.T) {
	var gotForm url.Values
	entra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+testTenant+"/oauth2/v2.0/token" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"x","token_type":"Bearer"}`))
	}))
	defer entra.Close()
	h := newHarness(t, map[string]string{EnvRedirectAllowed: "http://127.0.0.1,https://claude.ai/api/mcp/auth_callback"}, &Options{EntraBase: entra.URL})

	// AS metadata advertises PKCE.
	resp, _ := http.Get(h.srv.URL + "/.well-known/oauth-authorization-server")
	var md map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&md)
	resp.Body.Close()
	if ccm, _ := md["code_challenge_methods_supported"].([]any); len(ccm) != 1 || ccm[0] != "S256" {
		t.Fatalf("metadata = %v", md)
	}

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	q := url.Values{
		"response_type": {"code"}, "client_id": {"client-123"}, "redirect_uri": {"http://127.0.0.1:3334/cb"},
		"code_challenge": {"abc"}, "code_challenge_method": {"S256"}, "state": {"s"},
		"resource": {"https://mail-mcp.example.com/mcp"}, "scope": {"api://whatever/x openid"},
	}
	resp, _ = noRedirect.Get(h.srv.URL + "/oauth/authorize?" + q.Encode())
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), entra.URL+"/"+testTenant+"/oauth2/v2.0/authorize?") {
		t.Fatalf("redirect = %s", loc)
	}
	if loc.Query().Has("resource") {
		t.Fatal("resource must be stripped for Entra")
	}
	if s := loc.Query().Get("scope"); s != "api://"+testAudience+"/Mail.Read.MCP openid offline_access" {
		t.Fatalf("scope = %q", s)
	}

	for name, mut := range map[string]func(url.Values){
		"no PKCE":          func(v url.Values) { v.Del("code_challenge") },
		"plain PKCE":       func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"other client":     func(v url.Values) { v.Set("client_id", "evil") },
		"foreign redirect": func(v url.Values) { v.Set("redirect_uri", "https://evil.example.com/cb") },
	} {
		v := url.Values{}
		for k, vv := range q {
			v[k] = append([]string(nil), vv...)
		}
		mut(v)
		resp, _ := noRedirect.Get(h.srv.URL + "/oauth/authorize?" + v.Encode())
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d", name, resp.StatusCode)
		}
	}

	form := url.Values{"grant_type": {"authorization_code"}, "code": {"c"}, "code_verifier": {"v"},
		"client_id": {"client-123"}, "redirect_uri": {"http://127.0.0.1:3334/cb"}, "resource": {"https://mail-mcp.example.com/mcp"}}
	resp, err := http.PostForm(h.srv.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "access_token") {
		t.Fatalf("token: %d %s", resp.StatusCode, body)
	}
	if gotForm.Has("resource") || gotForm.Get("code_verifier") != "v" {
		t.Fatalf("forwarded form = %v", gotForm)
	}
	if strings.Contains(h.audit.String(), `"c"`) || strings.Contains(h.audit.String(), "code_verifier") {
		t.Fatal("token request must not be logged")
	}

	// Static client registration.
	resp, _ = http.Post(h.srv.URL+"/oauth/register", "application/json",
		strings.NewReader(`{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`))
	var reg map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || reg["client_id"] != "client-123" || reg["token_endpoint_auth_method"] != "none" {
		t.Fatalf("register: %d %v", resp.StatusCode, reg)
	}
	resp, _ = http.Post(h.srv.URL+"/oauth/register", "application/json",
		strings.NewReader(`{"redirect_uris":["https://evil.example.com/cb"]}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign redirect registration status = %d", resp.StatusCode)
	}
}

func TestAllMailboxAccessEndToEnd(t *testing.T) {
	const romeoOID = "00000000-0000-0000-0000-00000000000d"
	h := newHarness(t, map[string]string{
		EnvAllowedUsers:   thomasOID + "," + romeoOID + ",hugo@example.com",
		EnvMailboxGrants:  thomasOID + "=*;" + romeoOID + "=*",
		EnvMailboxDomains: "example.com",
	}, nil)
	romeo, err := h.connect(t, h.jwks.sign(t, validClaims(romeoOID, "romeo@example.com")))
	if err != nil {
		t.Fatal(err)
	}
	defer romeo.Close()

	who, isErr := call(t, romeo, "whoami", nil)
	if isErr || who["all_mailboxes"] != true {
		t.Fatalf("whoami: %v", who)
	}
	out, isErr := call(t, romeo, "mail_get", map[string]any{"mailbox": "Somebody.Else@example.com", "id": "m1"})
	if isErr || out["mailbox"] != "somebody.else@example.com" {
		t.Fatalf("all-mailbox read: %v", out)
	}
	if _, isErr = call(t, romeo, "mail_list", map[string]any{"mailbox": "x@other.com"}); !isErr {
		t.Fatal("mailbox outside MCP_MAILBOX_DOMAINS must be refused")
	}

	hugo, err := h.connect(t, h.jwks.sign(t, validClaims(hugoOID, "hugo@example.com")))
	if err != nil {
		t.Fatal(err)
	}
	defer hugo.Close()
	if _, isErr = call(t, hugo, "mail_list", map[string]any{"mailbox": "somebody.else@example.com"}); !isErr {
		t.Fatal("a user without * must stay on their own mailbox")
	}

	found := false
	for _, line := range strings.Split(strings.TrimSpace(h.audit.String()), "\n") {
		var e map[string]any
		_ = json.Unmarshal([]byte(line), &e)
		if e["tool"] == "mail_get" && e["user_oid"] == romeoOID && e["mailbox"] == "somebody.else@example.com" && e["outcome"] == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit must record the target mailbox:\n%s", h.audit.String())
	}
	for _, c := range *h.calls {
		if strings.Contains(c, "other.com") {
			t.Fatalf("Graph called for a refused mailbox: %s", c)
		}
	}
}
