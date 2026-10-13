package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/skylarbpayne/msgcli/internal/auth"
	"github.com/spf13/cobra"
)

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show authentication status",
	RunE:  runAuthStatus,
}

func init() {
	authCmd.AddCommand(authStatusCmd)
}

type statusOutput struct {
	ConfigExists bool            `json:"config_exists"`
	ClientID     string          `json:"client_id,omitempty"`
	Accounts     []accountStatus `json:"accounts"`
}

type accountStatus struct {
	Alias     string `json:"alias"`
	Email     string `json:"email"`
	ExpiresAt string `json:"expires_at"`
	Valid     bool   `json:"valid"`
	Error     string `json:"error,omitempty"`
}

type appStatusOutput struct {
	Mode       string `json:"mode"`
	TenantID   string `json:"tenant_id"`
	ClientID   string `json:"client_id"`
	Mailbox    string `json:"mailbox,omitempty"`
	TokenValid bool   `json:"token_valid"`
	Error      string `json:"error,omitempty"`
}

func runAuthStatus(cmd *cobra.Command, args []string) error {
	if auth.AppAuthEnabled() {
		return runAuthStatusApp()
	}

	status := statusOutput{}

	// Check config
	config, err := auth.LoadConfig()
	if err != nil {
		status.ConfigExists = false
	} else {
		status.ConfigExists = true
		status.ClientID = config.ClientID[:8] + "..." // Partially mask
	}

	// List accounts
	accounts, err := auth.ListAccounts()
	if err != nil {
		accounts = []auth.AccountInfo{}
	}

	ctx := context.Background()
	for _, acc := range accounts {
		as := accountStatus{
			Alias: acc.Alias,
			Email: acc.Email,
		}

		token, err := auth.LoadToken(acc.Alias)
		if err != nil {
			as.Error = err.Error()
		} else {
			as.ExpiresAt = time.Unix(token.ExpiresAt, 0).Format(time.RFC3339)

			// Try to validate token (or refresh if needed)
			if status.ConfigExists {
				_, err := auth.GetValidToken(ctx, acc.Alias)
				if err != nil {
					as.Valid = false
					as.Error = err.Error()
				} else {
					as.Valid = true
				}
			}
		}

		status.Accounts = append(status.Accounts, as)
	}

	format := GetOutputFormat()
	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Table format
	if !status.ConfigExists {
		Infof("Configuration: NOT SET")
		Infof("Run 'msgcli auth setup' to configure")
		return nil
	}

	fmt.Printf("Configuration: OK (client_id: %s)\n", status.ClientID)
	fmt.Println()

	if len(status.Accounts) == 0 {
		Infof("No accounts configured. Run 'msgcli auth add <alias>' to add one.")
		return nil
	}

	fmt.Printf("%-12s %-30s %-10s %s\n", "ALIAS", "EMAIL", "VALID", "EXPIRES")
	fmt.Printf("%-12s %-30s %-10s %s\n", "-----", "-----", "-----", "-------")
	for _, acc := range status.Accounts {
		validStr := "✗"
		if acc.Valid {
			validStr = "✓"
		}
		expiry := acc.ExpiresAt
		if expiry == "" {
			expiry = "unknown"
		}
		fmt.Printf("%-12s %-30s %-10s %s\n", acc.Alias, acc.Email, validStr, expiry)
		if acc.Error != "" {
			fmt.Printf("             Error: %s\n", acc.Error)
		}
	}

	return nil
}

// runAuthStatusApp reports status for app-only (client credentials) mode and
// validates the credentials by requesting a token.
func runAuthStatusApp() error {
	status := appStatusOutput{
		Mode:    "app",
		Mailbox: os.Getenv(auth.EnvMailbox),
	}

	creds, err := auth.AppCredentialsFromEnv()
	if err != nil {
		status.Error = err.Error()
	} else {
		status.TenantID = mask(creds.TenantID)
		status.ClientID = mask(creds.ClientID)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := creds.GetToken(ctx); err != nil {
			status.Error = err.Error()
		} else {
			status.TokenValid = true
		}
	}

	if GetOutputFormat() == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	fmt.Println("Mode: app-only (client credentials from environment)")
	if status.Error != "" {
		fmt.Printf("Token: ✗ (%s)\n", status.Error)
		return nil
	}
	fmt.Printf("Tenant ID: %s\n", status.TenantID)
	fmt.Printf("Client ID: %s\n", status.ClientID)
	if status.Mailbox != "" {
		fmt.Printf("Default mailbox: %s\n", status.Mailbox)
	} else {
		fmt.Println("Default mailbox: not set (pass --account <email> or set " + auth.EnvMailbox + ")")
	}
	fmt.Println("Token: ✓")
	return nil
}

// mask shortens an identifier for display.
func mask(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "..."
}
