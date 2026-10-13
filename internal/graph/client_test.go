package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/skylarbpayne/msgcli/internal/auth"
)

// testClient returns a Client pointed at a test server with a stubbed token.
func testClient(serverURL string) *Client {
	return &Client{
		httpClient: &http.Client{},
		account:    "test",
		base:       "/me",
		baseURL:    serverURL,
		token: func(ctx context.Context) (string, error) {
			return "test-token", nil
		},
	}
}

func TestGetPagedFollowsNextLink(t *testing.T) {
	var requests []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.String())
		page := r.URL.Query().Get("page")
		resp := ListResponse[Message]{
			Value: []Message{{ID: page + "-1"}, {ID: page + "-2"}},
		}
		if page != "3" {
			nextPage := "2"
			if page == "2" {
				nextPage = "3"
			}
			resp.NextLink = server.URL + "/me/messages?page=" + nextPage
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := testClient(server.URL)

	t.Run("stops at limit mid-page", func(t *testing.T) {
		requests = nil
		result, err := GetPaged[Message](t.Context(), c, "/me/messages?page=1", 3)
		if err != nil {
			t.Fatalf("GetPaged failed: %v", err)
		}
		if len(result.Value) != 3 {
			t.Fatalf("got %d items, want 3", len(result.Value))
		}
		if len(requests) != 2 {
			t.Fatalf("made %d requests, want 2: %v", len(requests), requests)
		}
	})

	t.Run("collects all pages when limit exceeds total", func(t *testing.T) {
		requests = nil
		result, err := GetPaged[Message](t.Context(), c, "/me/messages?page=1", 100)
		if err != nil {
			t.Fatalf("GetPaged failed: %v", err)
		}
		if len(result.Value) != 6 {
			t.Fatalf("got %d items, want 6", len(result.Value))
		}
		if len(requests) != 3 {
			t.Fatalf("made %d requests, want 3: %v", len(requests), requests)
		}
	})

	t.Run("limit zero fetches a single page", func(t *testing.T) {
		requests = nil
		result, err := GetPaged[Message](t.Context(), c, "/me/messages?page=1", 0)
		if err != nil {
			t.Fatalf("GetPaged failed: %v", err)
		}
		if len(result.Value) != 2 {
			t.Fatalf("got %d items, want 2", len(result.Value))
		}
		if len(requests) != 1 {
			t.Fatalf("made %d requests, want 1: %v", len(requests), requests)
		}
	})
}

func TestListMessagesClampsPageSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != fmt.Sprint(maxPageSize) {
			t.Errorf("$top = %q, want %d", got, maxPageSize)
		}
		json.NewEncoder(w).Encode(ListResponse[Message]{})
	}))
	defer server.Close()

	c := testClient(server.URL)
	if _, err := c.ListMessages(t.Context(), "inbox", &QueryParams{Top: 2500}); err != nil {
		t.Fatalf("ListMessages failed: %v", err)
	}
}

func TestListAttachments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/messages/msg-1/attachments" {
			t.Errorf("path = %q, want /me/messages/msg-1/attachments", r.URL.Path)
		}
		if got := r.URL.Query().Get("$select"); got == "" {
			t.Error("expected a $select to avoid fetching contentBytes in listings")
		}
		json.NewEncoder(w).Encode(ListResponse[Attachment]{Value: []Attachment{
			{ID: "att-1", Name: "report.pdf", ContentType: "application/pdf", Size: 12345},
		}})
	}))
	defer server.Close()

	c := testClient(server.URL)
	result, err := c.ListAttachments(t.Context(), "msg-1")
	if err != nil {
		t.Fatalf("ListAttachments failed: %v", err)
	}
	if len(result.Value) != 1 || result.Value[0].Name != "report.pdf" {
		t.Fatalf("unexpected attachments: %+v", result.Value)
	}
}

func TestDownloadAttachment(t *testing.T) {
	raw := []byte("%PDF-1.7 raw bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/messages/msg-1/attachments/att-1/$value" {
			t.Errorf("path = %q, want the attachment $value endpoint", r.URL.Path)
		}
		w.Write(raw)
	}))
	defer server.Close()

	c := testClient(server.URL)
	data, err := c.DownloadAttachment(t.Context(), "msg-1", "att-1")
	if err != nil {
		t.Fatalf("DownloadAttachment failed: %v", err)
	}
	if string(data) != string(raw) {
		t.Fatalf("data = %q, want raw bytes untouched", data)
	}
}

func TestDraftWithAttachmentsFlow(t *testing.T) {
	var server *httptest.Server
	var uploads []string
	var uploaded []byte
	sendCalled := false
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/me/messages":
			var m Message
			json.NewDecoder(r.Body).Decode(&m)
			if m.Subject != "test" || len(m.ToRecipients) != 2 {
				t.Errorf("draft payload wrong: %+v", m)
			}
			m.ID = "draft-1"
			json.NewEncoder(w).Encode(m)
		case r.Method == "POST" && r.URL.Path == "/me/messages/draft-1/attachments":
			var att map[string]any
			json.NewDecoder(r.Body).Decode(&att)
			if att["@odata.type"] != "#microsoft.graph.fileAttachment" || att["name"] != "small.xlsx" {
				t.Errorf("small attachment payload wrong: %v", att["name"])
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "att-1"})
		case r.Method == "POST" && r.URL.Path == "/me/messages/draft-1/attachments/createUploadSession":
			json.NewEncoder(w).Encode(map[string]string{"uploadUrl": server.URL + "/upload/xyz"})
		case r.Method == "PUT" && r.URL.Path == "/upload/xyz":
			if r.Header.Get("Authorization") != "" {
				t.Error("upload session PUT must not carry Authorization")
			}
			uploads = append(uploads, r.Header.Get("Content-Range"))
			b, _ := io.ReadAll(r.Body)
			uploaded = append(uploaded, b...)
			if len(uploads) == 3 {
				w.WriteHeader(201)
			} else {
				w.WriteHeader(200)
				json.NewEncoder(w).Encode(map[string]any{"nextExpectedRanges": []string{"next"}})
			}
		case r.Method == "POST" && r.URL.Path == "/me/messages/draft-1/send":
			sendCalled = true
			w.WriteHeader(202)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()

	c := testClient(server.URL)
	draft, err := c.CreateDraft(t.Context(), &Message{
		Subject:      "test",
		Body:         &ItemBody{ContentType: "text", Content: "hi"},
		ToRecipients: []Recipient{{EmailAddress{Address: "a@b.c"}}, {EmailAddress{Address: "d@e.f"}}},
	})
	if err != nil || draft.ID != "draft-1" {
		t.Fatalf("CreateDraft: %v %+v", err, draft)
	}

	if err := c.AddAttachment(t.Context(), "draft-1", "small.xlsx", "application/vnd.ms-excel", []byte("data")); err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}

	old := uploadChunkSize
	uploadChunkSize = 320 * 1024
	defer func() { uploadChunkSize = old }()
	big := bytes.Repeat([]byte("x"), 320*1024*2+100) // 3 chunks
	if err := c.UploadLargeAttachment(t.Context(), "draft-1", "big.zip", big); err != nil {
		t.Fatalf("UploadLargeAttachment: %v", err)
	}
	want := []string{
		"bytes 0-327679/655460",
		"bytes 327680-655359/655460",
		"bytes 655360-655459/655460",
	}
	if len(uploads) != 3 || uploads[0] != want[0] || uploads[1] != want[1] || uploads[2] != want[2] {
		t.Fatalf("Content-Range sequence = %v, want %v", uploads, want)
	}
	if !bytes.Equal(uploaded, big) {
		t.Fatalf("uploaded bytes differ: %d vs %d", len(uploaded), len(big))
	}

	if err := c.SendDraft(t.Context(), "draft-1"); err != nil || !sendCalled {
		t.Fatalf("SendDraft: %v (called=%v)", err, sendCalled)
	}
}

func TestListFoldersPaginate(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.URL.Query().Get("$top"); got == "" {
			t.Error("folder listings should set $top to avoid the 10-item default page")
		}
		json.NewEncoder(w).Encode(ListResponse[MailFolder]{Value: []MailFolder{{ID: "f1", DisplayName: "Sabrina"}}})
	}))
	defer server.Close()

	c := testClient(server.URL)
	if _, err := c.ListMailFolders(t.Context()); err != nil {
		t.Fatalf("ListMailFolders failed: %v", err)
	}
	if paths[0] != "/me/mailFolders" {
		t.Fatalf("path = %q, want /me/mailFolders", paths[0])
	}

	if _, err := c.ListChildFolders(t.Context(), "f1"); err != nil {
		t.Fatalf("ListChildFolders failed: %v", err)
	}
	if paths[1] != "/me/mailFolders/f1/childFolders" {
		t.Fatalf("path = %q, want /me/mailFolders/f1/childFolders", paths[1])
	}
}

func TestSearchMessagesClampsPageSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != fmt.Sprint(maxSearchPageSize) {
			t.Errorf("$top = %q, want %d", got, maxSearchPageSize)
		}
		if got := r.URL.Query().Get("$search"); got != `"invoice"` {
			t.Errorf("$search = %q, want quoted query", got)
		}
		json.NewEncoder(w).Encode(ListResponse[Message]{})
	}))
	defer server.Close()

	c := testClient(server.URL)
	if _, err := c.SearchMessages(t.Context(), "invoice", 999); err != nil {
		t.Fatalf("SearchMessages failed: %v", err)
	}
}

func TestNewClientBasePath(t *testing.T) {
	t.Run("delegated mode uses /me", func(t *testing.T) {
		t.Setenv(auth.EnvTenantID, "")
		t.Setenv(auth.EnvClientID, "")
		t.Setenv(auth.EnvClientSecret, "")
		c := NewClient("work")
		if c.base != "/me" {
			t.Fatalf("base = %q, want /me", c.base)
		}
	})

	t.Run("app mode uses /users/mailbox", func(t *testing.T) {
		t.Setenv(auth.EnvTenantID, "t")
		t.Setenv(auth.EnvClientID, "c")
		t.Setenv(auth.EnvClientSecret, "s")
		c := NewClient("contact@e-novacorp.com")
		if c.base != "/users/contact@e-novacorp.com" {
			t.Fatalf("base = %q, want /users/contact@e-novacorp.com", c.base)
		}
	})
}
