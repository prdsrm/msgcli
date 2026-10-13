package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/skylarbpayne/msgcli/internal/auth"
	"github.com/skylarbpayne/msgcli/internal/graph"
	"github.com/spf13/cobra"
)

var (
	viewOut         string
	viewNoOpen      bool
	viewAttachments bool
)

var mailViewCmd = &cobra.Command{
	Use:   "view <message-id>",
	Short: "Render a message as HTML and open it in the default browser",
	Long: `Fetch a message, write its body to an HTML file (inline images embedded),
and open it in the default browser.

With --attachments, the message's attachments are downloaded next to the
HTML file, linked in a bar at the top of the page, and opened too.

Use --out to choose the file location and --no-open to only write the files.`,
	Args: cobra.ExactArgs(1),
	RunE: runMailView,
}

func init() {
	mailViewCmd.Flags().StringVar(&viewOut, "out", "", "Write the HTML to this file (default: a temp file)")
	mailViewCmd.Flags().BoolVar(&viewNoOpen, "no-open", false, "Only write the files, don't open anything")
	mailViewCmd.Flags().BoolVarP(&viewAttachments, "attachments", "A", false, "Also download the message's attachments, link them in the page, and open them")
	mailCmd.AddCommand(mailViewCmd)
}

// attachmentLink is a downloaded attachment referenced from the rendered page.
type attachmentLink struct {
	Name string
	Path string
}

func runMailView(cmd *cobra.Command, args []string) error {
	messageID := args[0]

	account, err := auth.ResolveAccount(GetAccountFlag())
	if err != nil {
		return err
	}

	client := graph.NewClient(account)
	ctx := context.Background()

	msg, err := client.GetMessage(ctx, messageID)
	if err != nil {
		return err
	}

	content := renderMessageHTML(msg)

	var attachments []graph.Attachment
	if msg.HasAttachments || strings.Contains(content, "cid:") || viewAttachments {
		resp, err := client.ListAttachmentsWithContent(ctx, messageID)
		if err != nil {
			Infof("Warning: couldn't fetch attachments: %v", err)
		} else {
			attachments = resp.Value
		}
	}

	// Embed inline images (cid: references) as data URIs so the page is
	// self-contained.
	for _, att := range attachments {
		cid := strings.Trim(att.ContentID, "<>")
		if cid == "" || att.ContentBytes == "" {
			continue
		}
		dataURI := "data:" + att.ContentType + ";base64," + att.ContentBytes
		content = strings.ReplaceAll(content, "cid:"+cid, dataURI)
	}

	var links []attachmentLink
	if viewAttachments {
		links, err = saveViewAttachments(attachments)
		if err != nil {
			return err
		}
		if len(links) > 0 {
			content = injectAttachmentsBar(content, links)
		}
	}

	target := viewOut
	if target == "" {
		f, err := os.CreateTemp("", "msgcli-mail-*.html")
		if err != nil {
			return err
		}
		f.Close()
		target = f.Name()
	}

	if err := os.WriteFile(target, []byte(content), 0600); err != nil {
		return err
	}

	fmt.Println(target)
	for _, l := range links {
		fmt.Println(l.Path)
	}

	if viewNoOpen {
		return nil
	}
	if err := openInBrowser(target); err != nil {
		return err
	}
	for _, l := range links {
		if err := openInBrowser(l.Path); err != nil {
			Infof("Warning: couldn't open %s: %v", l.Path, err)
		}
	}
	return nil
}

// saveViewAttachments writes the message's downloadable attachments to disk:
// next to --out when set, otherwise into a fresh temp directory.
func saveViewAttachments(attachments []graph.Attachment) ([]attachmentLink, error) {
	dir := ""
	if viewOut != "" {
		dir = filepath.Dir(viewOut)
	}

	var links []attachmentLink
	seen := make(map[string]int)
	for _, att := range attachments {
		if att.IsInline {
			continue
		}
		if att.ContentBytes == "" {
			Infof("Skipping %q: no downloadable content (item or reference attachment)", att.Name)
			continue
		}
		data, err := base64.StdEncoding.DecodeString(att.ContentBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to decode %q: %w", att.Name, err)
		}

		if dir == "" {
			dir, err = os.MkdirTemp("", "msgcli-att-")
			if err != nil {
				return nil, err
			}
		}

		name := sanitizeFilename(att.Name)
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%d_%s", seen[name], name)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			return nil, err
		}
		links = append(links, attachmentLink{Name: att.Name, Path: path})
	}
	return links, nil
}

// injectAttachmentsBar adds a bar with file:// links to the downloaded
// attachments, inside the document's <body> when there is one.
func injectAttachmentsBar(doc string, links []attachmentLink) string {
	var b strings.Builder
	b.WriteString(`<div style="font-family:sans-serif;font-size:14px;padding:10px 12px;background:#fff8e1;border-bottom:1px solid #e0c97f">&#128206; Attachments: `)
	for i, l := range links {
		if i > 0 {
			b.WriteString(" &middot; ")
		}
		fmt.Fprintf(&b, `<a href="%s">%s</a>`, fileURL(l.Path), html.EscapeString(l.Name))
	}
	b.WriteString("</div>")
	bar := b.String()

	lower := strings.ToLower(doc)
	if i := strings.Index(lower, "<body"); i >= 0 {
		if j := strings.Index(doc[i:], ">"); j >= 0 {
			pos := i + j + 1
			return doc[:pos] + bar + doc[pos:]
		}
	}
	return bar + doc
}

func fileURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

// renderMessageHTML builds a standalone HTML page for the message. Full HTML
// documents are kept as-is; fragments and plain text get a small header with
// subject/from/date.
func renderMessageHTML(msg *graph.Message) string {
	body := ""
	contentType := "text"
	if msg.Body != nil {
		body = msg.Body.Content
		contentType = msg.Body.ContentType
	}

	if contentType == "html" && strings.Contains(strings.ToLower(body), "<html") {
		return body
	}

	from := ""
	if msg.From != nil {
		from = msg.From.EmailAddress.Name + " <" + msg.From.EmailAddress.Address + ">"
	}
	header := fmt.Sprintf(
		`<div style="font-family:sans-serif;padding:12px;border-bottom:1px solid #ccc;background:#f7f7f7">
<div style="font-size:1.2em;font-weight:bold">%s</div>
<div>From: %s</div>
<div>Received: %s</div>
</div>`,
		html.EscapeString(msg.Subject),
		html.EscapeString(from),
		html.EscapeString(msg.ReceivedDateTime.Local().Format("2006-01-02 15:04")),
	)

	if contentType != "html" {
		body = "<pre style=\"white-space:pre-wrap;font-family:sans-serif;padding:12px\">" + html.EscapeString(body) + "</pre>"
	}

	return fmt.Sprintf("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>%s</title></head><body style=\"margin:0\">%s\n%s</body></html>",
		html.EscapeString(msg.Subject), header, body)
}

func openInBrowser(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}
