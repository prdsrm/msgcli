package cmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/skylarbpayne/msgcli/internal/auth"
	"github.com/skylarbpayne/msgcli/internal/graph"
	"github.com/skylarbpayne/msgcli/internal/mcpserver"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run msgcli as a remote MCP server",
}

var mcpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve read-only mail tools over MCP Streamable HTTP, behind Microsoft Entra ID sign-in",
	Long: `Serve read-only mail tools (mail_list, mail_get, attachment_list,
attachment_get, folders_list, whoami) over MCP Streamable HTTP at /mcp.

Callers must present a Microsoft Entra ID access token for this API and be on
MCP_ALLOWED_USERS. Each user reads only their own mailbox unless
MCP_MAILBOX_GRANTS says otherwise. Mail is read with the app-only credentials
in AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET.

There are no send, reply, forward, move or delete tools. See README
("MCP server") for configuration and Azure setup.`,
	RunE: runMCPServe,
}

func init() {
	mcpCmd.AddCommand(mcpServeCmd)
	rootCmd.AddCommand(mcpCmd)
}

func runMCPServe(cmd *cobra.Command, args []string) error {
	cfg, err := mcpserver.ConfigFromEnv()
	if err != nil {
		return err
	}
	creds, err := auth.AppCredentialsFromEnv()
	if err != nil {
		return err
	}
	if creds == nil {
		return errors.New("app credentials are required: set AZURE_TENANT_ID, AZURE_CLIENT_ID and AZURE_CLIENT_SECRET")
	}

	audit := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(slog.String("component", "msgcli-mcp"))
	readers := func(mailbox string) mcpserver.MailReader {
		return graph.NewMailboxClient(mailbox, creds.GetToken)
	}
	srv := mcpserver.New(cfg, readers, &mcpserver.Options{Audit: audit})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	audit.Info("listening", slog.String("addr", cfg.ListenAddr), slog.String("resource", cfg.ResourceURL()),
		slog.String("auth_mode", string(cfg.AuthMode)), slog.Int("allowed_users", len(cfg.AllowedUsers)))
	return srv.ListenAndServe(ctx)
}
