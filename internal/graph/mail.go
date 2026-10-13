package graph

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Message represents an Outlook email message
type Message struct {
	ID                string      `json:"id"`
	Subject           string      `json:"subject"`
	BodyPreview       string      `json:"bodyPreview"`
	Body              *ItemBody   `json:"body,omitempty"`
	From              *Recipient  `json:"from,omitempty"`
	ToRecipients      []Recipient `json:"toRecipients,omitempty"`
	CcRecipients      []Recipient `json:"ccRecipients,omitempty"`
	BccRecipients     []Recipient `json:"bccRecipients,omitempty"`
	ReceivedDateTime  time.Time   `json:"receivedDateTime"`
	SentDateTime      time.Time   `json:"sentDateTime,omitempty"`
	HasAttachments    bool        `json:"hasAttachments"`
	Importance        string      `json:"importance"`
	IsRead            bool        `json:"isRead"`
	IsDraft           bool        `json:"isDraft"`
	ConversationID    string      `json:"conversationId,omitempty"`
	ParentFolderID    string      `json:"parentFolderId,omitempty"`
	WebLink           string      `json:"webLink,omitempty"`
	InternetMessageID string      `json:"internetMessageId,omitempty"`
}

// ItemBody represents message body content
type ItemBody struct {
	ContentType string `json:"contentType"` // "text" or "html"
	Content     string `json:"content"`
}

// Recipient represents an email recipient
type Recipient struct {
	EmailAddress EmailAddress `json:"emailAddress"`
}

// EmailAddress represents an email address
type EmailAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// MailFolder represents a mail folder
type MailFolder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ParentFolderID   string `json:"parentFolderId,omitempty"`
	ChildFolderCount int    `json:"childFolderCount"`
	UnreadItemCount  int    `json:"unreadItemCount"`
	TotalItemCount   int    `json:"totalItemCount"`
}

// Attachment represents a message attachment. ContentID and ContentBytes are
// only populated when fetched with content (see ListAttachmentsWithContent).
type Attachment struct {
	ODataType    string `json:"@odata.type,omitempty"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	Size         int    `json:"size"`
	IsInline     bool   `json:"isInline"`
	ContentID    string `json:"contentId,omitempty"`
	ContentBytes string `json:"contentBytes,omitempty"` // base64, fileAttachment only
}

// SendMailRequest is the request body for sending mail
type SendMailRequest struct {
	Message         Message `json:"message"`
	SaveToSentItems bool    `json:"saveToSentItems"`
}

// ListMessages lists messages in a folder. When params.Top exceeds the API's
// per-page maximum, additional pages are fetched automatically until Top
// messages have been collected or the folder is exhausted.
func (c *Client) ListMessages(ctx context.Context, folder string, params *QueryParams) (*ListResponse[Message], error) {
	if folder == "" {
		folder = "inbox"
	}

	limit := 0
	if params != nil {
		limit = params.Top
		if params.Top > maxPageSize {
			paged := *params
			paged.Top = maxPageSize
			params = &paged
		}
	}

	path := fmt.Sprintf("%s/mailFolders/%s/messages", c.base, folder)
	if params != nil {
		path += params.ToQuery()
	}

	return GetPaged[Message](ctx, c, path, limit)
}

// GetMessage retrieves a single message by ID
func (c *Client) GetMessage(ctx context.Context, messageID string) (*Message, error) {
	path := fmt.Sprintf("%s/messages/%s", c.base, messageID)

	var result Message
	if err := c.Get(ctx, path, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// SendMail sends an email message
func (c *Client) SendMail(ctx context.Context, to []string, cc []string, subject, body string, isHTML bool) error {
	contentType := "text"
	if isHTML {
		contentType = "html"
	}

	toRecipients := make([]Recipient, len(to))
	for i, addr := range to {
		toRecipients[i] = Recipient{EmailAddress: EmailAddress{Address: addr}}
	}

	ccRecipients := make([]Recipient, len(cc))
	for i, addr := range cc {
		ccRecipients[i] = Recipient{EmailAddress: EmailAddress{Address: addr}}
	}

	// Send only the fields that are set: Graph rejects the zero values the
	// full Message struct would serialize (e.g. an empty importance).
	message := map[string]any{
		"subject":      subject,
		"body":         &ItemBody{ContentType: contentType, Content: body},
		"toRecipients": toRecipients,
	}
	if len(ccRecipients) > 0 {
		message["ccRecipients"] = ccRecipients
	}
	req := map[string]any{
		"message":         message,
		"saveToSentItems": true,
	}

	return c.Post(ctx, c.base+"/sendMail", req, nil)
}

// CreateDraft creates a draft message and returns it (including its ID).
// Only the fields that are set are sent: Graph rejects the zero values the
// full Message struct would serialize (e.g. an empty importance).
func (c *Client) CreateDraft(ctx context.Context, msg *Message) (*Message, error) {
	payload := map[string]any{"subject": msg.Subject}
	if msg.Body != nil {
		payload["body"] = msg.Body
	}
	if len(msg.ToRecipients) > 0 {
		payload["toRecipients"] = msg.ToRecipients
	}
	if len(msg.CcRecipients) > 0 {
		payload["ccRecipients"] = msg.CcRecipients
	}
	if len(msg.BccRecipients) > 0 {
		payload["bccRecipients"] = msg.BccRecipients
	}

	var result Message
	if err := c.Post(ctx, c.base+"/messages", payload, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AddAttachment attaches a small file (< 3 MB) to a draft message.
func (c *Client) AddAttachment(ctx context.Context, messageID, name, contentType string, data []byte) error {
	body := map[string]any{
		"@odata.type":  "#microsoft.graph.fileAttachment",
		"name":         name,
		"contentType":  contentType,
		"contentBytes": base64.StdEncoding.EncodeToString(data),
	}
	path := fmt.Sprintf("%s/messages/%s/attachments", c.base, messageID)
	return c.Post(ctx, path, body, nil)
}

// uploadChunkSize is the upload-session chunk size; Graph requires a multiple
// of 320 KiB. Overridable in tests.
var uploadChunkSize = 10 * 320 * 1024

// UploadLargeAttachment attaches a file of any size (up to ~150 MB) to a
// draft via an upload session, streaming it in chunks.
func (c *Client) UploadLargeAttachment(ctx context.Context, messageID, name string, data []byte) error {
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	body := map[string]any{"AttachmentItem": map[string]any{
		"attachmentType": "file", "name": name, "size": len(data),
	}}
	path := fmt.Sprintf("%s/messages/%s/attachments/createUploadSession", c.base, messageID)
	if err := c.Post(ctx, path, body, &session); err != nil {
		return err
	}
	if session.UploadURL == "" {
		return fmt.Errorf("upload session for %s returned no uploadUrl", name)
	}

	total := len(data)
	for start := 0; start < total; start += uploadChunkSize {
		end := min(start+uploadChunkSize, total)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, session.UploadURL, bytes.NewReader(data[start:end]))
		if err != nil {
			return err
		}
		// The upload URL is pre-authenticated: no Authorization header.
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, total))
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("chunk upload of %s failed: HTTP %d: %s", name, resp.StatusCode, respBody[:min(len(respBody), 200)])
		}
	}
	return nil
}

// SendDraft sends an existing draft message.
func (c *Client) SendDraft(ctx context.Context, messageID string) error {
	path := fmt.Sprintf("%s/messages/%s/send", c.base, messageID)
	return c.Post(ctx, path, nil, nil)
}

// ReplyToMessage sends a reply to a message
func (c *Client) ReplyToMessage(ctx context.Context, messageID, comment string) error {
	path := fmt.Sprintf("%s/messages/%s/reply", c.base, messageID)
	body := map[string]string{"comment": comment}
	return c.Post(ctx, path, body, nil)
}

// ReplyAllToMessage sends a reply-all to a message
func (c *Client) ReplyAllToMessage(ctx context.Context, messageID, comment string) error {
	path := fmt.Sprintf("%s/messages/%s/replyAll", c.base, messageID)
	body := map[string]string{"comment": comment}
	return c.Post(ctx, path, body, nil)
}

// ForwardMessage forwards a message
func (c *Client) ForwardMessage(ctx context.Context, messageID, comment string, to []string) error {
	path := fmt.Sprintf("%s/messages/%s/forward", c.base, messageID)

	toRecipients := make([]Recipient, len(to))
	for i, addr := range to {
		toRecipients[i] = Recipient{EmailAddress: EmailAddress{Address: addr}}
	}

	body := map[string]interface{}{
		"comment":      comment,
		"toRecipients": toRecipients,
	}
	return c.Post(ctx, path, body, nil)
}

// DeleteMessage deletes a message
func (c *Client) DeleteMessage(ctx context.Context, messageID string) error {
	path := fmt.Sprintf("%s/messages/%s", c.base, messageID)
	return c.Delete(ctx, path)
}

// MoveMessage moves a message to a different folder
func (c *Client) MoveMessage(ctx context.Context, messageID, destinationFolderID string) (*Message, error) {
	path := fmt.Sprintf("%s/messages/%s/move", c.base, messageID)
	body := map[string]string{"destinationId": destinationFolderID}

	var result Message
	if err := c.Post(ctx, path, body, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// MarkAsRead marks a message as read or unread
func (c *Client) MarkAsRead(ctx context.Context, messageID string, isRead bool) error {
	path := fmt.Sprintf("%s/messages/%s", c.base, messageID)
	body := map[string]bool{"isRead": isRead}
	return c.Patch(ctx, path, body, nil)
}

// ListMailFolders lists top-level mail folders, following pages (the API
// defaults to 10 folders per page).
func (c *Client) ListMailFolders(ctx context.Context) (*ListResponse[MailFolder], error) {
	return GetPaged[MailFolder](ctx, c, c.base+"/mailFolders?$top=100", 1000)
}

// ListChildFolders lists the child folders of a folder (by ID or well-known
// name).
func (c *Client) ListChildFolders(ctx context.Context, folderID string) (*ListResponse[MailFolder], error) {
	path := fmt.Sprintf("%s/mailFolders/%s/childFolders?$top=100", c.base, folderID)
	return GetPaged[MailFolder](ctx, c, path, 1000)
}

// GetMailFolder gets a mail folder by ID or well-known name
func (c *Client) GetMailFolder(ctx context.Context, folderID string) (*MailFolder, error) {
	path := fmt.Sprintf("%s/mailFolders/%s", c.base, folderID)

	var result MailFolder
	if err := c.Get(ctx, path, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// ListAttachments lists a message's attachments (metadata only — $select
// keeps the base64 content out of the response).
func (c *Client) ListAttachments(ctx context.Context, messageID string) (*ListResponse[Attachment], error) {
	path := fmt.Sprintf("%s/messages/%s/attachments?$select=id,name,contentType,size,isInline", c.base, messageID)
	return GetPaged[Attachment](ctx, c, path, 500)
}

// ListAttachmentsWithContent lists a message's attachments including their
// base64 content and contentId (used to resolve inline cid: images).
func (c *Client) ListAttachmentsWithContent(ctx context.Context, messageID string) (*ListResponse[Attachment], error) {
	path := fmt.Sprintf("%s/messages/%s/attachments", c.base, messageID)
	return GetPaged[Attachment](ctx, c, path, 500)
}

// DownloadAttachment returns the raw bytes of a file or item attachment via
// the /$value endpoint (item attachments come back as MIME). Reference
// attachments (OneDrive links) have no downloadable content.
func (c *Client) DownloadAttachment(ctx context.Context, messageID, attachmentID string) ([]byte, error) {
	path := fmt.Sprintf("%s/messages/%s/attachments/%s/$value", c.base, messageID, attachmentID)
	return c.GetRaw(ctx, path)
}

// SearchMessages searches the whole mailbox using KQL, following pages until
// top results have been collected or there are no more matches.
func (c *Client) SearchMessages(ctx context.Context, query string, top int) (*ListResponse[Message], error) {
	params := &QueryParams{
		Search: query,
		Top:    min(top, maxSearchPageSize),
	}

	path := c.base + "/messages" + params.ToQuery()

	return GetPaged[Message](ctx, c, path, top)
}
