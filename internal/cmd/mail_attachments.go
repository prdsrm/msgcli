package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/skylarbpayne/msgcli/internal/auth"
	"github.com/skylarbpayne/msgcli/internal/graph"
	"github.com/spf13/cobra"
)

var (
	attachDownloadID  string
	attachDownloadAll bool
	attachOut         string
	attachForce       bool
)

var mailAttachmentsCmd = &cobra.Command{
	Use:   "attachments <message-id>",
	Short: "List or download attachments of a message",
	Long: `List a message's attachments, or download them.

Without flags, lists attachment metadata (ID, name, type, size).
Use --download <attachment-id> to save one attachment, or --download-all
to save all of them.`,
	Args: cobra.ExactArgs(1),
	RunE: runMailAttachments,
}

func init() {
	mailAttachmentsCmd.Flags().StringVarP(&attachDownloadID, "download", "d", "", "Attachment ID to download")
	mailAttachmentsCmd.Flags().BoolVar(&attachDownloadAll, "download-all", false, "Download all attachments")
	mailAttachmentsCmd.Flags().StringVar(&attachOut, "out", "", "Output file (with --download) or directory (with --download-all); defaults to the attachment name in the current directory")
	mailAttachmentsCmd.Flags().BoolVar(&attachForce, "force", false, "Overwrite existing files")
	mailCmd.AddCommand(mailAttachmentsCmd)
}

type savedAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	Size int    `json:"size"`
}

func runMailAttachments(cmd *cobra.Command, args []string) error {
	messageID := args[0]

	if attachDownloadID != "" && attachDownloadAll {
		return fmt.Errorf("--download and --download-all are mutually exclusive")
	}

	account, err := auth.ResolveAccount(GetAccountFlag())
	if err != nil {
		return err
	}

	client := graph.NewClient(account)
	ctx := context.Background()

	result, err := client.ListAttachments(ctx, messageID)
	if err != nil {
		return err
	}

	if attachDownloadID == "" && !attachDownloadAll {
		return printAttachmentList(result.Value)
	}

	var toDownload []graph.Attachment
	if attachDownloadAll {
		toDownload = result.Value
	} else {
		for _, att := range result.Value {
			if att.ID == attachDownloadID {
				toDownload = append(toDownload, att)
				break
			}
		}
		if len(toDownload) == 0 {
			return fmt.Errorf("attachment %q not found on message", attachDownloadID)
		}
	}

	var saved []savedAttachment
	for _, att := range toDownload {
		if att.ODataType == "#microsoft.graph.referenceAttachment" {
			Infof("Skipping %q: reference attachments (cloud links) have no downloadable content", att.Name)
			continue
		}

		target, err := attachmentTarget(att)
		if err != nil {
			return err
		}

		data, err := client.DownloadAttachment(ctx, messageID, att.ID)
		if err != nil {
			return fmt.Errorf("failed to download %q: %w", att.Name, err)
		}

		if err := os.WriteFile(target, data, 0600); err != nil {
			return err
		}
		saved = append(saved, savedAttachment{ID: att.ID, Name: att.Name, Path: target, Size: len(data)})
	}

	if GetOutputFormat() == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(saved)
	}
	for _, s := range saved {
		fmt.Printf("Saved %s (%d bytes)\n", s.Path, s.Size)
	}
	return nil
}

// attachmentTarget resolves and validates the output path for one attachment.
func attachmentTarget(att graph.Attachment) (string, error) {
	name := sanitizeFilename(att.Name)

	var target string
	switch {
	case attachDownloadAll:
		dir := attachOut
		if dir == "" {
			dir = "."
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", err
		}
		target = filepath.Join(dir, name)
	case attachOut != "":
		target = attachOut
	default:
		target = name
	}

	if !attachForce {
		if _, err := os.Stat(target); err == nil {
			return "", fmt.Errorf("%s already exists - use --force to overwrite", target)
		}
	}
	return target, nil
}

// sanitizeFilename strips path components and characters that could escape
// the target directory; attachment names come from untrusted senders.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', 0:
			return '_'
		default:
			return r
		}
	}, name)
	if name == "" || name == "." || name == ".." {
		name = "attachment"
	}
	return name
}

func printAttachmentList(attachments []graph.Attachment) error {
	if GetOutputFormat() == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(attachments)
	}

	if len(attachments) == 0 {
		Infof("No attachments")
		return nil
	}

	fmt.Printf("%-40s %-30s %10s  %s\n", "NAME", "TYPE", "SIZE", "ID")
	for _, att := range attachments {
		inline := ""
		if att.IsInline {
			inline = " (inline)"
		}
		fmt.Printf("%-40s %-30s %10d  %s%s\n", truncate(att.Name, 40), truncate(att.ContentType, 30), att.Size, att.ID, inline)
	}
	return nil
}
