package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMailboxClientScopesPathsAndPrefersText(t *testing.T) {
	var gotPath, gotPrefer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotPrefer = r.Header.Get("Prefer")
		_ = json.NewEncoder(w).Encode(Message{ID: "m1", Body: &ItemBody{ContentType: "text", Content: "hi"}})
	}))
	defer srv.Close()

	c := NewMailboxClient("hugo@example.com", func(context.Context) (string, error) { return "t", nil }).WithBaseURL(srv.URL)
	m, err := c.GetMessageBodyAsText(t.Context(), "AAMk=")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/users/hugo@example.com/messages/AAMk=" {
		t.Errorf("path = %s", gotPath)
	}
	if gotPrefer != `outlook.body-content-type="text"` {
		t.Errorf("Prefer = %q", gotPrefer)
	}
	if m.Body.Content != "hi" {
		t.Errorf("body = %+v", m.Body)
	}
}
