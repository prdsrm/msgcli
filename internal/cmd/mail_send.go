package cmd

import (
	"bufio"
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/skylarbpayne/msgcli/internal/auth"
	"github.com/skylarbpayne/msgcli/internal/graph"
	"github.com/spf13/cobra"
)

var (
	mailSendTo      []string
	mailSendCc      []string
	mailSendSubject string
	mailSendBody    string
	mailSendHTML    bool
	mailSendStdin   bool
	mailSendAttach  []string
	mailSendDraft   bool
)

var mailSendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send an email message",
	Long: `Send an email message.

The body can be provided via --body flag, --stdin flag (reads from stdin),
or interactively if neither is specified.`,
	RunE: runMailSend,
}

func init() {
	mailSendCmd.Flags().StringSliceVar(&mailSendTo, "to", nil, "Recipient email addresses (required)")
	mailSendCmd.Flags().StringSliceVar(&mailSendCc, "cc", nil, "CC email addresses")
	mailSendCmd.Flags().StringVar(&mailSendSubject, "subject", "", "Email subject (required)")
	mailSendCmd.Flags().StringVar(&mailSendBody, "body", "", "Email body")
	mailSendCmd.Flags().BoolVar(&mailSendHTML, "html", false, "Treat body as HTML")
	mailSendCmd.Flags().BoolVar(&mailSendStdin, "stdin", false, "Read body from stdin")
	mailSendCmd.Flags().StringSliceVar(&mailSendAttach, "attach", nil, "File(s) to attach (repeatable; large files are uploaded in chunks)")
	mailSendCmd.Flags().BoolVar(&mailSendDraft, "draft", false, "Create the message as a draft (with attachments) and print its ID instead of sending")

	mailSendCmd.MarkFlagRequired("to")
	mailSendCmd.MarkFlagRequired("subject")

	mailCmd.AddCommand(mailSendCmd)
}

// smallAttachmentLimit is the Graph limit for inline fileAttachment POSTs;
// bigger files go through an upload session.
const smallAttachmentLimit = 3 * 1024 * 1024

func toRecipients(addrs []string) []graph.Recipient {
	out := make([]graph.Recipient, len(addrs))
	for i, a := range addrs {
		out[i] = graph.Recipient{EmailAddress: graph.EmailAddress{Address: a}}
	}
	return out
}

// sendWithAttachments creates a draft, uploads every attachment, then sends
// it (unless draftOnly). Returns the draft ID.
func sendWithAttachments(ctx context.Context, client *graph.Client, body string, draftOnly bool) (string, error) {
	contentType := "text"
	if mailSendHTML {
		contentType = "html"
	}
	draft, err := client.CreateDraft(ctx, &graph.Message{
		Subject:      mailSendSubject,
		Body:         &graph.ItemBody{ContentType: contentType, Content: body},
		ToRecipients: toRecipients(mailSendTo),
		CcRecipients: toRecipients(mailSendCc),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create draft: %w", err)
	}

	for _, path := range mailSendAttach {
		data, err := os.ReadFile(path)
		if err != nil {
			return draft.ID, err
		}
		name := filepath.Base(path)
		ctype := mime.TypeByExtension(filepath.Ext(path))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		if len(data) <= smallAttachmentLimit {
			err = client.AddAttachment(ctx, draft.ID, name, ctype, data)
		} else {
			Infof("Uploading %s (%d MB) in chunks...", name, len(data)/1048576)
			err = client.UploadLargeAttachment(ctx, draft.ID, name, data)
		}
		if err != nil {
			return draft.ID, fmt.Errorf("failed to attach %s (draft %s kept): %w", name, draft.ID, err)
		}
		Infof("Attached %s (%d bytes)", name, len(data))
	}

	if draftOnly {
		return draft.ID, nil
	}
	if err := client.SendDraft(ctx, draft.ID); err != nil {
		return draft.ID, fmt.Errorf("failed to send draft %s: %w", draft.ID, err)
	}
	return draft.ID, nil
}

func runMailSend(cmd *cobra.Command, args []string) error {
	if len(mailSendTo) == 0 {
		return fmt.Errorf("at least one --to recipient is required")
	}

	if mailSendSubject == "" {
		return fmt.Errorf("--subject is required")
	}

	// Get body content
	body := mailSendBody

	if mailSendStdin {
		// Read from stdin
		var sb strings.Builder
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			sb.WriteString(scanner.Text())
			sb.WriteString("\n")
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("error reading stdin: %w", err)
		}
		body = strings.TrimSuffix(sb.String(), "\n")
	} else if body == "" && !IsNoInput() {
		// Interactive input
		Infof("Enter message body (Ctrl+D to finish):")
		var sb strings.Builder
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			sb.WriteString(scanner.Text())
			sb.WriteString("\n")
		}
		body = strings.TrimSuffix(sb.String(), "\n")
	}

	if body == "" {
		return fmt.Errorf("message body is required")
	}

	account, err := auth.ResolveAccount(GetAccountFlag())
	if err != nil {
		return err
	}

	client := graph.NewClient(account)
	ctx := context.Background()

	if len(mailSendAttach) > 0 || mailSendDraft {
		draftID, err := sendWithAttachments(ctx, client, body, mailSendDraft)
		if err != nil {
			return err
		}
		if mailSendDraft {
			Infof("Draft created (not sent)")
			fmt.Println(draftID)
		} else {
			Infof("Message sent successfully (%d attachments)", len(mailSendAttach))
		}
		return nil
	}

	if err := client.SendMail(ctx, mailSendTo, mailSendCc, mailSendSubject, body, mailSendHTML); err != nil {
		return fmt.Errorf("failed to send mail: %w", err)
	}

	Infof("Message sent successfully")
	return nil
}
