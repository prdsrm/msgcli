package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/skylarbpayne/msgcli/internal/graph"
)

const (
	testTenant   = "11111111-2222-3333-4444-555555555555"
	testAudience = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	thomasOID    = "00000000-0000-0000-0000-00000000000a"
	hugoOID      = "00000000-0000-0000-0000-00000000000b"
	outsiderOID  = "00000000-0000-0000-0000-00000000000c"
)

// fakeJWKS serves a JWKS with one RSA key and signs tokens with it.
type fakeJWKS struct {
	key    *rsa.PrivateKey
	kid    string
	server *httptest.Server
	hits   int
	mu     sync.Mutex
}

func newFakeJWKS(t *testing.T) *fakeJWKS {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeJWKS{key: k, kid: "test-kid"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "kid": f.kid,
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		}}})
	}))
	t.Cleanup(f.server.Close)
	return f
}

// claims returns valid v2 delegated-token claims for oid/upn.
func validClaims(oid, upn string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":   "https://login.microsoftonline.com/" + testTenant + "/v2.0",
		"aud":   testAudience,
		"tid":   testTenant,
		"oid":   oid,
		"upn":   upn,
		"name":  "Test User",
		"scp":   "Mail.Read.MCP",
		"ver":   "2.0",
		"iat":   now.Unix(),
		"nbf":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"idtyp": "user",
	}
}

func (f *fakeJWKS) sign(t *testing.T, c jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testConfig(t *testing.T, extra map[string]string) *Config {
	t.Helper()
	env := map[string]string{
		EnvPublicURL:     "https://mail-mcp.example.com",
		EnvTenantID:      testTenant,
		EnvAudiences:     testAudience + ",api://msgcli-mcp",
		EnvAllowedUsers:  thomasOID + ",hugo@example.com",
		EnvOAuthClientID: "client-123",
	}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := configFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeReader is an in-memory MailReader recording the mailbox it is bound to.
type fakeReader struct {
	mailbox string
	calls   *[]string
}

func (f fakeReader) rec(s string) { *f.calls = append(*f.calls, f.mailbox+":"+s) }

func (f fakeReader) SearchMessages(_ context.Context, q string, top int) (*graph.ListResponse[graph.Message], error) {
	f.rec("search:" + q)
	return &graph.ListResponse[graph.Message]{Value: []graph.Message{{ID: "m1", Subject: "found " + q}}}, nil
}

func (f fakeReader) ListMessages(_ context.Context, folder string, p *graph.QueryParams) (*graph.ListResponse[graph.Message], error) {
	f.rec("list:" + folder + ":" + p.Filter)
	return &graph.ListResponse[graph.Message]{Value: []graph.Message{{
		ID: "m1", Subject: "Hello", From: &graph.Recipient{EmailAddress: graph.EmailAddress{Name: "A", Address: "a@x.com"}},
	}}}, nil
}

func (f fakeReader) GetMessageBodyAsText(_ context.Context, id string) (*graph.Message, error) {
	f.rec("get:" + id)
	return &graph.Message{ID: id, Subject: "S", Body: &graph.ItemBody{ContentType: "text", Content: "body text"}}, nil
}

func (f fakeReader) ListAttachments(_ context.Context, id string) (*graph.ListResponse[graph.Attachment], error) {
	f.rec("attachments:" + id)
	return &graph.ListResponse[graph.Attachment]{Value: []graph.Attachment{{ID: "a1", Name: "f.txt", ContentType: "text/plain", Size: 5}}}, nil
}

func (f fakeReader) GetAttachmentMeta(_ context.Context, id, aid string) (*graph.Attachment, error) {
	f.rec("attmeta:" + aid)
	if aid == "big" {
		return &graph.Attachment{ID: aid, Name: "big.bin", ContentType: "application/octet-stream", Size: maxAttachmentBytes + 1}, nil
	}
	if aid == "bin" {
		return &graph.Attachment{ID: aid, Name: "x.pdf", ContentType: "application/pdf", Size: 3}, nil
	}
	return &graph.Attachment{ID: aid, Name: "f.txt", ContentType: "text/plain", Size: 5}, nil
}

func (f fakeReader) DownloadAttachment(_ context.Context, id, aid string) ([]byte, error) {
	f.rec("download:" + aid)
	if aid == "bin" {
		return []byte{0xff, 0x00, 0x01}, nil
	}
	return []byte("hello"), nil
}

func (f fakeReader) ListMailFolders(context.Context) (*graph.ListResponse[graph.MailFolder], error) {
	f.rec("folders")
	return &graph.ListResponse[graph.MailFolder]{Value: []graph.MailFolder{{ID: "inbox-id", DisplayName: "Inbox", TotalItemCount: 3}}}, nil
}

func (f fakeReader) ListChildFolders(_ context.Context, id string) (*graph.ListResponse[graph.MailFolder], error) {
	f.rec("children:" + id)
	return &graph.ListResponse[graph.MailFolder]{}, nil
}
