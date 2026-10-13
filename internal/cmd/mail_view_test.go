package cmd

import (
	"strings"
	"testing"
)

func TestInjectAttachmentsBar(t *testing.T) {
	links := []attachmentLink{{Name: "CV.pdf", Path: "/tmp/CV.pdf"}}

	t.Run("inserts inside body tag of full documents", func(t *testing.T) {
		doc := `<html><head></head><BODY style="margin:0"><p>mail</p></BODY></html>`
		got := injectAttachmentsBar(doc, links)
		bodyEnd := strings.Index(got, `style="margin:0">`) + len(`style="margin:0">`)
		if !strings.HasPrefix(got[bodyEnd:], "<div") {
			t.Fatalf("bar not inserted right after body tag: %q", got)
		}
		if !strings.Contains(got, "CV.pdf") || !strings.Contains(got, "file:///tmp/CV.pdf") {
			t.Fatalf("bar missing link: %q", got)
		}
	})

	t.Run("prepends when no body tag", func(t *testing.T) {
		got := injectAttachmentsBar("<p>fragment</p>", links)
		if !strings.HasPrefix(got, "<div") || !strings.HasSuffix(got, "<p>fragment</p>") {
			t.Fatalf("bar not prepended: %q", got)
		}
	})

	t.Run("escapes attachment names", func(t *testing.T) {
		got := injectAttachmentsBar("<p>x</p>", []attachmentLink{{Name: "a<b>.pdf", Path: "/tmp/a.pdf"}})
		if strings.Contains(got, "a<b>") {
			t.Fatalf("name not escaped: %q", got)
		}
	})
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"CV.pdf":             "CV.pdf",
		"../../etc/passwd":   "passwd",
		"a/b\\c:d.pdf":       "b_c_d.pdf",
		"..":                 "attachment",
		"":                   "attachment",
		"Loïc ROUGIER (1).p": "Loïc ROUGIER (1).p",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
