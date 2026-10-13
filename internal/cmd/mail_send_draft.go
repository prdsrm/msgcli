package cmd

import (
	"context"
	"fmt"

	"github.com/skylarbpayne/msgcli/internal/auth"
	"github.com/skylarbpayne/msgcli/internal/graph"
	"github.com/spf13/cobra"
)

var mailSendDraftCmd = &cobra.Command{
	Use:   "send-draft <message-id>",
	Short: "Send an existing draft message",
	Long: `Send a draft previously created with 'mail send --draft'.

Inspect the draft first with 'mail get <id>' and 'mail attachments <id>'.`,
	Args: cobra.ExactArgs(1),
	RunE: runMailSendDraft,
}

func init() {
	mailCmd.AddCommand(mailSendDraftCmd)
}

func runMailSendDraft(cmd *cobra.Command, args []string) error {
	account, err := auth.ResolveAccount(GetAccountFlag())
	if err != nil {
		return err
	}

	client := graph.NewClient(account)
	ctx := context.Background()

	if err := client.SendDraft(ctx, args[0]); err != nil {
		return fmt.Errorf("failed to send draft: %w", err)
	}

	Infof("Draft sent successfully")
	return nil
}
