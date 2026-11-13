package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skylarbpayne/msgcli/internal/graph"
)

// MailReader is the read-only Graph surface the MCP tools use. There is
// deliberately no send, reply, forward, delete, move or update method here:
// the server cannot change a mailbox even if a tool handler had a bug.
type MailReader interface {
	SearchMessages(ctx context.Context, query string, top int) (*graph.ListResponse[graph.Message], error)
	ListMessages(ctx context.Context, folder string, params *graph.QueryParams) (*graph.ListResponse[graph.Message], error)
	GetMessageBodyAsText(ctx context.Context, messageID string) (*graph.Message, error)
	ListAttachments(ctx context.Context, messageID string) (*graph.ListResponse[graph.Attachment], error)
	GetAttachmentMeta(ctx context.Context, messageID, attachmentID string) (*graph.Attachment, error)
	DownloadAttachment(ctx context.Context, messageID, attachmentID string) ([]byte, error)
	ListMailFolders(ctx context.Context) (*graph.ListResponse[graph.MailFolder], error)
	ListChildFolders(ctx context.Context, folderID string) (*graph.ListResponse[graph.MailFolder], error)
}

// ReaderFactory returns a MailReader bound to one mailbox.
type ReaderFactory func(mailbox string) MailReader

const (
	maxListLimit       = 100
	defaultListLimit   = 25
	maxBodyChars       = 100_000
	maxAttachmentBytes = 10 << 20 // 10 MiB per attachment_get
)

// Graph IDs (messages, attachments, folders) are URL-safe base64-like
// strings. Anything else (slashes, dots, query strings) is rejected so an ID
// can never rewrite the Graph path to reach another mailbox.
var graphIDRe = regexp.MustCompile(`^[A-Za-z0-9_\-=+]{1,512}$`)

func checkID(field, v string) error {
	if !graphIDRe.MatchString(v) {
		return fmt.Errorf("invalid %s", field)
	}
	return nil
}

// Tools wires the read-only tools to a policy, a Graph reader and an audit log.
type Tools struct {
	Policy *Policy
	Reader ReaderFactory
	Audit  *slog.Logger
}

// --- inputs / outputs ---

type MailboxArg struct {
	Mailbox string `json:"mailbox,omitempty" jsonschema:"mailbox to read; defaults to your own. Other mailboxes only if explicitly granted"`
}

type MailListIn struct {
	MailboxArg
	Query  string `json:"query,omitempty" jsonschema:"KQL search across the whole mailbox, e.g. from:alice@example.com subject:invoice. Cannot be combined with folder/after/before"`
	Folder string `json:"folder,omitempty" jsonschema:"folder to list when no query: inbox (default), sentitems, drafts, archive, deleteditems, junkemail, or a folder ID"`
	After  string `json:"after,omitempty" jsonschema:"only messages received on/after this date (YYYY-MM-DD or RFC3339)"`
	Before string `json:"before,omitempty" jsonschema:"only messages received before this date (YYYY-MM-DD or RFC3339)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max messages, 1-100 (default 25)"`
}

type MessageSummary struct {
	ID             string   `json:"id"`
	Subject        string   `json:"subject"`
	From           string   `json:"from,omitempty"`
	To             []string `json:"to,omitempty"`
	ReceivedAt     string   `json:"received_at"`
	IsRead         bool     `json:"is_read"`
	HasAttachments bool     `json:"has_attachments"`
	Preview        string   `json:"preview,omitempty"`
}

type MailListOut struct {
	Mailbox  string           `json:"mailbox"`
	Messages []MessageSummary `json:"messages"`
}

type MailGetIn struct {
	MailboxArg
	ID string `json:"id" jsonschema:"message ID from mail_list"`
}

type MailGetOut struct {
	Mailbox        string   `json:"mailbox"`
	ID             string   `json:"id"`
	Subject        string   `json:"subject"`
	From           string   `json:"from,omitempty"`
	To             []string `json:"to,omitempty"`
	Cc             []string `json:"cc,omitempty"`
	ReceivedAt     string   `json:"received_at"`
	SentAt         string   `json:"sent_at,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty"`
	HasAttachments bool     `json:"has_attachments"`
	Body           string   `json:"body"`
	BodyTruncated  bool     `json:"body_truncated,omitempty"`
	WebLink        string   `json:"web_link,omitempty"`
}

type AttachmentListIn struct {
	MailboxArg
	MessageID string `json:"message_id" jsonschema:"message ID"`
}

type AttachmentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	IsInline    bool   `json:"is_inline"`
}

type AttachmentListOut struct {
	Mailbox     string           `json:"mailbox"`
	Attachments []AttachmentInfo `json:"attachments"`
}

type AttachmentGetIn struct {
	MailboxArg
	MessageID    string `json:"message_id" jsonschema:"message ID"`
	AttachmentID string `json:"attachment_id" jsonschema:"attachment ID from attachment_list"`
}

type AttachmentGetOut struct {
	Mailbox string `json:"mailbox"`
	AttachmentInfo
	ContentBase64 string `json:"content_base64,omitempty"`
	Text          string `json:"text,omitempty"`
}

type FoldersListIn struct {
	MailboxArg
	Parent string `json:"parent,omitempty" jsonschema:"list the child folders of this folder (ID or well-known name); default: top level"`
}

type FolderInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Unread      int    `json:"unread"`
	Total       int    `json:"total"`
	HasChildren bool   `json:"has_children"`
}

type FoldersListOut struct {
	Mailbox string       `json:"mailbox"`
	Folders []FolderInfo `json:"folders"`
}

type WhoAmIOut struct {
	ObjectID  string   `json:"object_id"`
	UPN       string   `json:"upn"`
	Name      string   `json:"name,omitempty"`
	Mailboxes []string `json:"mailboxes"`
}

// Register adds the read-only tools to the server.
func (t *Tools) Register(s *mcp.Server) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	mcp.AddTool(s, &mcp.Tool{Name: "whoami", Description: "Show who you are signed in as and which mailboxes you may read.", Annotations: ro}, t.whoami)
	mcp.AddTool(s, &mcp.Tool{Name: "mail_list", Description: "List or search messages (newest first). Read-only.", Annotations: ro}, t.mailList)
	mcp.AddTool(s, &mcp.Tool{Name: "mail_get", Description: "Read one message, body as plain text. Read-only.", Annotations: ro}, t.mailGet)
	mcp.AddTool(s, &mcp.Tool{Name: "attachment_list", Description: "List a message's attachments. Read-only.", Annotations: ro}, t.attachmentList)
	mcp.AddTool(s, &mcp.Tool{Name: "attachment_get", Description: "Download one attachment (max 10 MiB): text for text types, base64 otherwise. Read-only.", Annotations: ro}, t.attachmentGet)
	mcp.AddTool(s, &mcp.Tool{Name: "folders_list", Description: "List mail folders. Read-only.", Annotations: ro}, t.foldersList)
}

func ptr[T any](v T) *T { return &v }

// begin authenticates the call, resolves the mailbox, and returns a reader
// plus a function that writes the audit entry.
func (t *Tools) begin(ctx context.Context, req *mcp.CallToolRequest, tool, mailbox string, args map[string]any) (*Identity, string, MailReader, func(error), error) {
	start := time.Now()
	var id *Identity
	if req != nil && req.Extra != nil {
		id = IdentityFrom(req.Extra.TokenInfo)
	}
	resolved := ""
	finish := func(err error) {
		attrs := []any{
			slog.String("tool", tool),
			slog.String("mailbox", resolved),
			slog.Any("args", args),
			slog.Duration("duration", time.Since(start)),
		}
		if id != nil {
			attrs = append(attrs, slog.String("user_oid", id.ObjectID), slog.String("user_upn", id.UPN))
		}
		if err != nil {
			attrs = append(attrs, slog.String("outcome", "error"), slog.String("error", err.Error()))
		} else {
			attrs = append(attrs, slog.String("outcome", "ok"))
		}
		t.Audit.LogAttrs(ctx, slog.LevelInfo, "tool_call", toAttrs(attrs)...)
	}
	if id == nil || !t.Policy.Allowed(id) {
		err := ErrForbidden
		finish(err)
		return nil, "", nil, nil, err
	}
	mbx, err := t.Policy.ResolveMailbox(id, mailbox)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			err = fmt.Errorf("%w: you may only read %s", ErrForbidden, strings.Join(t.Policy.Mailboxes(id), ", "))
		}
		finish(err)
		return nil, "", nil, nil, err
	}
	resolved = mbx
	return id, mbx, t.Reader(mbx), finish, nil
}

func toAttrs(a []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(a))
	for _, v := range a {
		out = append(out, v.(slog.Attr))
	}
	return out
}

func (t *Tools) whoami(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, WhoAmIOut, error) {
	id, _, _, done, err := t.begin(ctx, req, "whoami", "", nil)
	if err != nil {
		return nil, WhoAmIOut{}, err
	}
	done(nil)
	return nil, WhoAmIOut{ObjectID: id.ObjectID, UPN: id.UPN, Name: id.Name, Mailboxes: t.Policy.Mailboxes(id)}, nil
}

func (t *Tools) mailList(ctx context.Context, req *mcp.CallToolRequest, in MailListIn) (*mcp.CallToolResult, MailListOut, error) {
	args := map[string]any{"query": in.Query, "folder": in.Folder, "after": in.After, "before": in.Before, "limit": in.Limit}
	_, mbx, r, done, err := t.begin(ctx, req, "mail_list", in.Mailbox, args)
	if err != nil {
		return nil, MailListOut{}, err
	}
	out, err := func() (MailListOut, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = defaultListLimit
		}
		limit = min(limit, maxListLimit)
		var res *graph.ListResponse[graph.Message]
		var err error
		if in.Query != "" {
			if in.Folder != "" || in.After != "" || in.Before != "" {
				return MailListOut{}, errors.New("query cannot be combined with folder/after/before (Graph $search limitation)")
			}
			if strings.ContainsAny(in.Query, "\"\\") {
				return MailListOut{}, errors.New("query must not contain double quotes or backslashes")
			}
			res, err = r.SearchMessages(ctx, in.Query, limit)
		} else {
			folder := in.Folder
			if folder == "" {
				folder = "inbox"
			}
			if err := checkID("folder", folder); err != nil {
				return MailListOut{}, err
			}
			var clauses []string
			if in.After != "" {
				ts, err := parseDate(in.After)
				if err != nil {
					return MailListOut{}, err
				}
				clauses = append(clauses, "receivedDateTime ge "+ts)
			}
			if in.Before != "" {
				ts, err := parseDate(in.Before)
				if err != nil {
					return MailListOut{}, err
				}
				clauses = append(clauses, "receivedDateTime lt "+ts)
			}
			res, err = r.ListMessages(ctx, folder, &graph.QueryParams{
				Top:     limit,
				Filter:  strings.Join(clauses, " and "),
				OrderBy: "receivedDateTime desc",
				Select:  []string{"id", "subject", "from", "toRecipients", "receivedDateTime", "isRead", "hasAttachments", "bodyPreview"},
			})
		}
		if err != nil {
			return MailListOut{}, err
		}
		out := MailListOut{Mailbox: mbx, Messages: []MessageSummary{}}
		for _, m := range res.Value {
			out.Messages = append(out.Messages, MessageSummary{
				ID: m.ID, Subject: m.Subject, From: addr(m.From), To: addrs(m.ToRecipients),
				ReceivedAt: m.ReceivedDateTime.UTC().Format(time.RFC3339), IsRead: m.IsRead,
				HasAttachments: m.HasAttachments, Preview: m.BodyPreview,
			})
		}
		return out, nil
	}()
	done(err)
	return nil, out, err
}

func (t *Tools) mailGet(ctx context.Context, req *mcp.CallToolRequest, in MailGetIn) (*mcp.CallToolResult, MailGetOut, error) {
	_, mbx, r, done, err := t.begin(ctx, req, "mail_get", in.Mailbox, map[string]any{"id": in.ID})
	if err != nil {
		return nil, MailGetOut{}, err
	}
	out, err := func() (MailGetOut, error) {
		if err := checkID("id", in.ID); err != nil {
			return MailGetOut{}, err
		}
		m, err := r.GetMessageBodyAsText(ctx, in.ID)
		if err != nil {
			return MailGetOut{}, err
		}
		body := ""
		if m.Body != nil {
			body = m.Body.Content
		}
		truncated := false
		if len(body) > maxBodyChars {
			body = truncateUTF8(body, maxBodyChars)
			truncated = true
		}
		out := MailGetOut{
			Mailbox: mbx, ID: m.ID, Subject: m.Subject, From: addr(m.From),
			To: addrs(m.ToRecipients), Cc: addrs(m.CcRecipients),
			ReceivedAt:     m.ReceivedDateTime.UTC().Format(time.RFC3339),
			ConversationID: m.ConversationID, HasAttachments: m.HasAttachments,
			Body: body, BodyTruncated: truncated, WebLink: m.WebLink,
		}
		if !m.SentDateTime.IsZero() {
			out.SentAt = m.SentDateTime.UTC().Format(time.RFC3339)
		}
		return out, nil
	}()
	done(err)
	return nil, out, err
}

func (t *Tools) attachmentList(ctx context.Context, req *mcp.CallToolRequest, in AttachmentListIn) (*mcp.CallToolResult, AttachmentListOut, error) {
	_, mbx, r, done, err := t.begin(ctx, req, "attachment_list", in.Mailbox, map[string]any{"message_id": in.MessageID})
	if err != nil {
		return nil, AttachmentListOut{}, err
	}
	out, err := func() (AttachmentListOut, error) {
		if err := checkID("message_id", in.MessageID); err != nil {
			return AttachmentListOut{}, err
		}
		res, err := r.ListAttachments(ctx, in.MessageID)
		if err != nil {
			return AttachmentListOut{}, err
		}
		out := AttachmentListOut{Mailbox: mbx, Attachments: []AttachmentInfo{}}
		for _, a := range res.Value {
			out.Attachments = append(out.Attachments, attInfo(a))
		}
		return out, nil
	}()
	done(err)
	return nil, out, err
}

func (t *Tools) attachmentGet(ctx context.Context, req *mcp.CallToolRequest, in AttachmentGetIn) (*mcp.CallToolResult, AttachmentGetOut, error) {
	args := map[string]any{"message_id": in.MessageID, "attachment_id": in.AttachmentID}
	_, mbx, r, done, err := t.begin(ctx, req, "attachment_get", in.Mailbox, args)
	if err != nil {
		return nil, AttachmentGetOut{}, err
	}
	out, err := func() (AttachmentGetOut, error) {
		if err := checkID("message_id", in.MessageID); err != nil {
			return AttachmentGetOut{}, err
		}
		if err := checkID("attachment_id", in.AttachmentID); err != nil {
			return AttachmentGetOut{}, err
		}
		meta, err := r.GetAttachmentMeta(ctx, in.MessageID, in.AttachmentID)
		if err != nil {
			return AttachmentGetOut{}, err
		}
		if meta.Size > maxAttachmentBytes {
			return AttachmentGetOut{}, fmt.Errorf("attachment is %d bytes, over the %d byte limit", meta.Size, maxAttachmentBytes)
		}
		data, err := r.DownloadAttachment(ctx, in.MessageID, in.AttachmentID)
		if err != nil {
			return AttachmentGetOut{}, err
		}
		if len(data) > maxAttachmentBytes {
			return AttachmentGetOut{}, fmt.Errorf("attachment is over the %d byte limit", maxAttachmentBytes)
		}
		out := AttachmentGetOut{Mailbox: mbx, AttachmentInfo: attInfo(*meta)}
		if isTextType(meta.ContentType) && utf8.Valid(data) {
			out.Text = string(data)
		} else {
			out.ContentBase64 = base64.StdEncoding.EncodeToString(data)
		}
		return out, nil
	}()
	done(err)
	return nil, out, err
}

func (t *Tools) foldersList(ctx context.Context, req *mcp.CallToolRequest, in FoldersListIn) (*mcp.CallToolResult, FoldersListOut, error) {
	_, mbx, r, done, err := t.begin(ctx, req, "folders_list", in.Mailbox, map[string]any{"parent": in.Parent})
	if err != nil {
		return nil, FoldersListOut{}, err
	}
	out, err := func() (FoldersListOut, error) {
		var res *graph.ListResponse[graph.MailFolder]
		var err error
		if in.Parent != "" {
			if err := checkID("parent", in.Parent); err != nil {
				return FoldersListOut{}, err
			}
			res, err = r.ListChildFolders(ctx, in.Parent)
		} else {
			res, err = r.ListMailFolders(ctx)
		}
		if err != nil {
			return FoldersListOut{}, err
		}
		out := FoldersListOut{Mailbox: mbx, Folders: []FolderInfo{}}
		for _, f := range res.Value {
			out.Folders = append(out.Folders, FolderInfo{
				ID: f.ID, Name: f.DisplayName, Unread: f.UnreadItemCount,
				Total: f.TotalItemCount, HasChildren: f.ChildFolderCount > 0,
			})
		}
		return out, nil
	}()
	done(err)
	return nil, out, err
}

// --- helpers ---

func parseDate(s string) (string, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("invalid date %q: use YYYY-MM-DD or RFC3339", s)
}

func addr(r *graph.Recipient) string {
	if r == nil {
		return ""
	}
	return fmtAddr(r.EmailAddress)
}

func addrs(rs []graph.Recipient) []string {
	var out []string
	for _, r := range rs {
		out = append(out, fmtAddr(r.EmailAddress))
	}
	return out
}

func fmtAddr(e graph.EmailAddress) string {
	if e.Name != "" && !strings.EqualFold(e.Name, e.Address) {
		return e.Name + " <" + e.Address + ">"
	}
	return e.Address
}

func attInfo(a graph.Attachment) AttachmentInfo {
	return AttachmentInfo{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size, IsInline: a.IsInline}
}

func isTextType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") ||
		strings.Contains(ct, "xml") || strings.Contains(ct, "csv")
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
