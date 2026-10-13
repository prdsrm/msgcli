package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	accountFlag string
	outputFlag  string
	noInputFlag bool
)

var rootCmd = &cobra.Command{
	Use:   "msgcli",
	Short: "Agent-first CLI for Microsoft Outlook Mail and Calendar",
	Long: `msgcli is a command-line interface for Microsoft Graph API,
focused on Outlook Mail and Calendar operations.

Designed for AI agents with JSON-first output, multi-account support,
and secure credential storage.`,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&accountFlag, "account", "a", "", "Account alias to use (default: first configured); target mailbox address in app-credential mode")
	rootCmd.PersistentFlags().StringVarP(&outputFlag, "output", "o", "", "Output format: json or table (default: auto)")
	rootCmd.PersistentFlags().BoolVar(&noInputFlag, "no-input", false, "Never prompt for input, fail if input needed")
}

// GetAccountFlag returns the account flag value
func GetAccountFlag() string {
	return accountFlag
}

// GetOutputFormat returns the output format, auto-detecting if not specified
func GetOutputFormat() string {
	if outputFlag != "" {
		return outputFlag
	}
	// Auto-detect: JSON for pipes, table for terminals
	fi, _ := os.Stdout.Stat()
	if (fi.Mode() & os.ModeCharDevice) == 0 {
		return "json"
	}
	return "table"
}

// IsNoInput returns whether no-input mode is enabled
func IsNoInput() bool {
	return noInputFlag
}

// Errorf prints an error message to stderr
func Errorf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
}

// Infof prints an info message to stderr (for progress, not data)
func Infof(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
