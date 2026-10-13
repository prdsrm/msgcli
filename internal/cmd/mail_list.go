package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/skylarbpayne/msgcli/internal/auth"
	"github.com/skylarbpayne/msgcli/internal/graph"
	"github.com/spf13/cobra"
)

var (
	mailListFolder string
	mailListLimit  int
	mailListQuery  string
	mailListBefore string
	mailListAfter  string
	mailListFilter string
)

var mailListCmd = &cobra.Command{
	Use:   "list",
	Short: "List email messages",
	Long: `List email messages in a folder.

Well-known folder names: inbox, drafts, sentitems, deleteditems, archive, junkemail`,
	RunE: runMailList,
}

func init() {
	mailListCmd.Flags().StringVarP(&mailListFolder, "folder", "f", "inbox", "Folder to list (inbox, drafts, sentitems, etc.)")
	mailListCmd.Flags().IntVarP(&mailListLimit, "limit", "l", 25, "Maximum number of messages to return (paginated automatically beyond the API page size)")
	mailListCmd.Flags().StringVarP(&mailListQuery, "query", "q", "", "Search the mailbox (KQL syntax, e.g. 'from:alice@example.com', 'subject:invoice', or free text)")
	mailListCmd.Flags().StringVar(&mailListBefore, "before", "", "Only messages received before this date (YYYY-MM-DD or RFC3339)")
	mailListCmd.Flags().StringVar(&mailListAfter, "after", "", "Only messages received on/after this date (YYYY-MM-DD or RFC3339)")
	mailListCmd.Flags().StringVar(&mailListFilter, "filter", "", "Raw OData $filter expression (e.g. \"from/emailAddress/address eq 'a@b.com'\")")
	mailCmd.AddCommand(mailListCmd)
}

// parseDateArg accepts YYYY-MM-DD or RFC3339 and returns RFC3339 UTC.
func parseDateArg(s string) (string, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("invalid date %q - use YYYY-MM-DD or RFC3339 (2026-04-20T22:35:35Z)", s)
}

// buildListFilter combines --before, --after, and --filter into one OData
// $filter expression. The receivedDateTime clauses must come first: Exchange
// requires $orderby properties to appear at the start of the $filter, and
// listings are ordered by receivedDateTime.
func buildListFilter() (string, error) {
	var clauses []string
	if mailListBefore != "" {
		ts, err := parseDateArg(mailListBefore)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, "receivedDateTime lt "+ts)
	}
	if mailListAfter != "" {
		ts, err := parseDateArg(mailListAfter)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, "receivedDateTime ge "+ts)
	}
	if mailListFilter != "" {
		clauses = append(clauses, mailListFilter)
	}
	return strings.Join(clauses, " and "), nil
}

func runMailList(cmd *cobra.Command, args []string) error {
	account, err := auth.ResolveAccount(GetAccountFlag())
	if err != nil {
		return err
	}

	client := graph.NewClient(account)
	ctx := context.Background()

	filter, err := buildListFilter()
	if err != nil {
		return err
	}
	if mailListQuery != "" && filter != "" {
		return fmt.Errorf("--query cannot be combined with --before/--after/--filter ($search and $filter are mutually exclusive in the Graph API)")
	}

	var result *graph.ListResponse[graph.Message]

	if mailListQuery != "" {
		result, err = client.SearchMessages(ctx, mailListQuery, mailListLimit)
	} else {
		// Exchange rejects $orderby on properties absent from $filter, so
		// only keep the ordering when the filter constrains receivedDateTime
		// (--before/--after) or no filter is set.
		orderBy := "receivedDateTime desc"
		if filter != "" && mailListBefore == "" && mailListAfter == "" {
			orderBy = ""
		}
		params := &graph.QueryParams{
			Top:     mailListLimit,
			Filter:  filter,
			OrderBy: orderBy,
			Select:  []string{"id", "subject", "from", "receivedDateTime", "isRead", "hasAttachments", "bodyPreview"},
		}
		result, err = client.ListMessages(ctx, mailListFolder, params)
	}

	if err != nil {
		return err
	}

	format := GetOutputFormat()
	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result.Value)
	}

	// Table format
	if len(result.Value) == 0 {
		Infof("No messages found")
		return nil
	}

	for _, msg := range result.Value {
		readMarker := "•"
		if msg.IsRead {
			readMarker = " "
		}
		attachMarker := " "
		if msg.HasAttachments {
			attachMarker = "📎"
		}

		from := ""
		if msg.From != nil {
			from = msg.From.EmailAddress.Address
			if msg.From.EmailAddress.Name != "" {
				from = msg.From.EmailAddress.Name
			}
		}

		subject := msg.Subject
		if len(subject) > 50 {
			subject = subject[:47] + "..."
		}

		timeStr := msg.ReceivedDateTime.Local().Format("Jan 02 15:04")

		fmt.Printf("%s%s %-20s %-50s %s\n", readMarker, attachMarker, truncate(from, 20), subject, timeStr)
		fmt.Printf("   ID: %s\n", msg.ID)
	}

	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s + strings.Repeat(" ", maxLen-len(s))
	}
	return s[:maxLen-3] + "..."
}

// Helper for formatting relative time
func relativeTime(t time.Time) string {
	now := time.Now()
	diff := now.Sub(t)

	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	case diff < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(diff.Hours()/24))
	default:
		return t.Format("Jan 02")
	}
}
