# msgcli

> Agent-first CLI for Microsoft Outlook Mail & Calendar

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**msgcli** is a fast, script-friendly command-line interface for Microsoft Graph API. Built for AI agents and automation, inspired by [gogcli](https://github.com/steipete/gogcli).

```bash
# What's in my inbox?
$ msgcli mail list
• 📎 John Smith           RE: Project proposal              Jan 17 09:42
     ID: AAMkAGI2TG...

# Create a meeting
$ msgcli calendar create --subject "Sync" --start "2024-01-20T14:00" --end "2024-01-20T14:30" --attendees "team@company.com"
Event created successfully
ID: AAMkAGI2TG...

# Agent-friendly JSON output
$ msgcli mail list -o json | jq '.[0].subject'
"RE: Project proposal"
```

## Why msgcli?

- **Agent-first** — JSON output, `--no-input` flag, clean stdout/stderr separation
- **Multi-account** — Switch between personal and work Microsoft accounts
- **Secure** — Tokens stored in OS keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service)
- **Simple auth** — Device code flow, no client secrets, 5-minute setup
- **Full CRUD** — Mail and Calendar operations for real workflows

## Quick Start

### 1. Azure App Registration (5 minutes, one-time)

1. Open [Azure Portal → App registrations](https://portal.azure.com/#blade/Microsoft_AAD_RegisteredApps/ApplicationsListBlade)
2. Click **"New registration"**
3. Configure:
   - **Name:** `msgcli`
   - **Account types:** "Accounts in any organizational directory and personal Microsoft accounts"
   - **Redirect URI:** Leave blank
4. Click **Register**
5. Copy the **Application (client) ID**
6. Go to **Authentication** → Enable **"Allow public client flows"** → Save

> 📖 Detailed guide with screenshots: [docs/SETUP.md](docs/SETUP.md)

### 2. Install

```bash
# Option A: Go install
go install github.com/skylarbpayne/msgcli/cmd/msgcli@latest

# Option B: Build from source
git clone https://github.com/skylarbpayne/msgcli.git
cd msgcli
make build
./bin/msgcli --help
```

### 3. Authenticate

```bash
# Store your client ID (one-time)
msgcli auth setup
# Enter client ID: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx

# Add an account
msgcli auth add personal
# To sign in, open a browser and go to:
#   https://microsoft.com/devicelogin
# Enter the code: ABCD1234
```

### 4. Use It

```bash
# List recent emails
msgcli mail list

# Read a specific email
msgcli mail get AAMkAGI2TG...

# Send an email
msgcli mail send --to "someone@example.com" --subject "Hello" --body "Hi there!"

# List this week's events
msgcli calendar list

# Check someone's availability
msgcli calendar availability --emails "colleague@company.com" --start "2024-01-20" --end "2024-01-21"
```

## App-Only Auth (Client Credentials)

For headless/agent use against an organizational tenant, msgcli can authenticate
as an application instead of a user — no browser, no keyring, no `auth setup`.
Set these environment variables (e.g. via direnv or a `.env` file):

```bash
export AZURE_TENANT_ID="..."
export AZURE_CLIENT_ID="..."
export AZURE_CLIENT_SECRET="..."
export MSGCLI_MAILBOX="contact@example.com"   # optional default mailbox
```

When they are set, msgcli uses the OAuth2 client credentials grant
(`scope=https://graph.microsoft.com/.default`) and they take precedence over
any device-code accounts. Requirements on the Azure side:

1. In your app registration, add **application** permissions (not delegated):
   `Mail.Read` (or `Mail.ReadWrite`) and `Mail.Send`, then **Grant admin consent**.
2. Create a client secret under **Certificates & secrets**.

App tokens have no signed-in user, so every command targets an explicit
mailbox: pass `--account <email>` or set `MSGCLI_MAILBOX`.

```bash
msgcli auth status                                   # verify credentials
msgcli mail list --account contact@example.com      # read mail
msgcli mail send --account contact@example.com --to "someone@example.com" --subject "Hi" --body "Hello"
```

### Pagination and search

`mail list --limit N` transparently follows `@odata.nextLink` when N exceeds
the Graph API page size (max 1000 per page for listings, default 10), so
`--limit 5000` just works. Search the mailbox with `--query` (KQL syntax):

```bash
msgcli mail list --query "from:alice@example.com" --limit 500
msgcli mail list --query "subject:invoice hasAttachments:true"
```

> ⚠️ `--query` uses Graph `$search`, which the server caps at roughly 250–275
> results regardless of `--limit`, ranked by relevance — fine for lookups,
> wrong for exhaustive exports. To enumerate completely, use `--before`/
> `--after` (received date) and `--filter` (raw OData) instead, which paginate
> without limit; note they only scan one folder (`--folder`, default inbox):

```bash
msgcli mail list --before 2026-04-20 --limit 3000                       # walk back in time
msgcli mail list --after 2026-01-01 --filter "from/emailAddress/address eq 'a@b.com'"
msgcli mail list --folder <folder-id> --limit 5000                      # any folder, incl. nested ones by ID
```

Calendar commands are not supported in app-only mode.

## Commands

### Authentication

| Command | Description |
|---------|-------------|
| `msgcli auth setup` | Store Azure client ID (one-time) |
| `msgcli auth add <alias>` | Add account via device code flow |
| `msgcli auth list` | List configured accounts |
| `msgcli auth status` | Show auth status and token validity |
| `msgcli auth remove <alias>` | Remove an account |

### Mail

| Command | Description |
|---------|-------------|
| `msgcli mail list` | List messages (default: inbox, last 25) |
| `msgcli mail get <id>` | Read a specific message |
| `msgcli mail view <id>` | Render a message as HTML (inline images embedded) and open it in the browser (`--attachments` also downloads, links, and opens its attachments) |
| `msgcli mail attachments <id>` | List a message's attachments (`--download <att-id>` or `--download-all` to save them) |
| `msgcli mail send` | Send a new message |
| `msgcli mail reply <id>` | Reply to a message |
| `msgcli mail delete <id>` | Delete a message |
| `msgcli mail move <id>` | Move message to folder |
| `msgcli mail folders` | List mail folders |

```bash
# Examples
msgcli mail list --folder sentitems --limit 10
msgcli mail list --query "from:boss@company.com"
msgcli mail send --to "a@b.com" --cc "c@d.com" --subject "Update" --body "Here's the update..."
msgcli mail reply AAMkAGI2TG... --body "Thanks!" --all
```

### Calendar

| Command | Description |
|---------|-------------|
| `msgcli calendar list` | List events (default: next 7 days) |
| `msgcli calendar get <id>` | Get event details |
| `msgcli calendar create` | Create a new event |
| `msgcli calendar update <id>` | Update an event |
| `msgcli calendar delete <id>` | Delete an event |
| `msgcli calendar respond <id>` | Accept/decline/tentative |
| `msgcli calendar availability` | Check free/busy for users |

```bash
# Examples
msgcli calendar list --start 2024-01-15 --end 2024-01-31
msgcli calendar create --subject "1:1" --start "2024-01-20T10:00" --end "2024-01-20T10:30" --location "Zoom"
msgcli calendar respond AAMkAGI2TG... --response accept --comment "See you there!"
msgcli calendar delete AAMkAGI2TG... --cancel --comment "Rescheduling"
```

### Global Flags

```bash
-a, --account <alias>    # Use specific account (default: first configured)
-o, --output json|table  # Output format (default: auto-detect)
    --no-input           # Never prompt, fail if input needed
```

## For AI Agents

msgcli is designed for AI agent integration:

```bash
# Guaranteed non-interactive mode
msgcli mail list --no-input -o json

# Pipe content
echo "Meeting notes attached" | msgcli mail send --to "team@co.com" --subject "Notes" --stdin --no-input

# Check exit codes
msgcli mail list -o json && echo "Success" || echo "Failed"
```

**Exit codes:**
- `0` — Success
- `1` — Error (check stderr)
- `2` — Auth required

**Output separation:**
- `stdout` — Data (JSON or table)
- `stderr` — Progress, errors, prompts

## Multi-Account

```bash
# Add multiple accounts
msgcli auth add personal    # @outlook.com
msgcli auth add work        # @company.com

# Use specific account
msgcli mail list -a personal
msgcli calendar list -a work

# Check all accounts
msgcli auth status
```

## Environment Variables

| Variable | Description |
|----------|-------------|
| `MSGCLI_CLIENT_ID` | Azure client ID (alternative to `auth setup`) |
| `MSGCLI_KEYRING_PASSWORD` | Keyring password for headless/CI environments |
| `AZURE_TENANT_ID` | Tenant ID for app-only (client credentials) auth |
| `AZURE_CLIENT_ID` | Client ID for app-only auth |
| `AZURE_CLIENT_SECRET` | Client secret for app-only auth |
| `MSGCLI_MAILBOX` | Default target mailbox in app-only mode (overridden by `--account`) |

## Building

```bash
make build      # Build to ./bin/msgcli
make install    # Install to $GOPATH/bin
make test       # Run tests
make release    # Cross-compile for all platforms
```

## How It Works

1. **Auth**: Device code flow (no client secret needed) — you authenticate in a browser, msgcli stores refresh tokens in your OS keychain
2. **API**: Direct Microsoft Graph API calls with automatic token refresh
3. **Output**: JSON for machines, tables for humans (auto-detected based on TTY)

## Contributing

PRs welcome! Please open an issue first to discuss major changes.

## License

MIT

---

Built with ❤️ for AI agents everywhere.
