package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/skylarbpayne/msgcli/internal/auth"
	"github.com/skylarbpayne/msgcli/internal/graph"
	"github.com/spf13/cobra"
)

var mailFoldersCmd = &cobra.Command{
	Use:   "folders [parent-folder-id]",
	Short: "List mail folders (top-level, or children of the given folder)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runMailFolders,
}

func init() {
	mailCmd.AddCommand(mailFoldersCmd)
}

func runMailFolders(cmd *cobra.Command, args []string) error {
	account, err := auth.ResolveAccount(GetAccountFlag())
	if err != nil {
		return err
	}

	client := graph.NewClient(account)
	ctx := context.Background()

	var result *graph.ListResponse[graph.MailFolder]
	if len(args) == 1 {
		result, err = client.ListChildFolders(ctx, args[0])
	} else {
		result, err = client.ListMailFolders(ctx)
	}
	if err != nil {
		return fmt.Errorf("failed to list folders: %w", err)
	}

	format := GetOutputFormat()
	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result.Value)
	}

	// Table format
	fmt.Printf("%-40s %-25s %8s %8s %9s\n", "ID", "NAME", "UNREAD", "TOTAL", "CHILDREN")
	fmt.Printf("%-40s %-25s %8s %8s %9s\n", "----", "----", "------", "-----", "--------")
	for _, folder := range result.Value {
		id := folder.ID
		if len(id) > 40 {
			id = id[:37] + "..."
		}
		fmt.Printf("%-40s %-25s %8d %8d %9d\n", id, folder.DisplayName, folder.UnreadItemCount, folder.TotalItemCount, folder.ChildFolderCount)
	}

	return nil
}
