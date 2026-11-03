# slack-cli

A command-line interface for Slack that uses your browser session tokens — no bot or Slack app setup required. Built with Go.

## How It Works

Uses the same authentication as the Slack web client: an `xoxc-` API token paired with an `xoxd-` session cookie. You paste the cookie once from your browser, and the CLI auto-extracts the API token.

## Install

```bash
go install github.com/max7866/slack-cli@latest
```

Or build from source:
```bash
git clone https://github.com/max7866/slack-cli.git
cd slack-cli
go build -o slack-cli .
```

## Setup

### Quick Setup (recommended)

```bash
slack-cli auth login
```

This will:
1. Open Slack in your browser
2. Prompt you to paste the `d` cookie from DevTools (`Cmd+Option+I` -> Application -> Cookies -> `app.slack.com` -> `d`)
3. Prompt for your browser's User-Agent (paste from Console: `navigator.userAgent`)
4. Auto-extract the API token from your session
5. Save credentials to `~/.slack-cli/config.json`

> **SSO/Okta users:** The User-Agent step is important. Slack SSO workspaces detect when a session is used from a different client and will invalidate it. Providing your browser's exact User-Agent ensures the CLI looks like the same session.

### Manual Setup

If you already have both tokens:
```bash
slack-cli auth setup
```

### Verify

```bash
slack-cli auth test
```

## Usage

```bash
# List channels
slack-cli channels list
slack-cli channels list -a          # include archived
slack-cli channels list --mine      # only channels you created
slack-cli channels favorites        # your starred channels

# Read messages
slack-cli messages read #general
slack-cli messages read #general -n 50
slack-cli messages read @username
slack-cli messages read @ana,@ben,@carol           # read a group DM

# Read a thread (the "(N replies — ts ...)" hint on a `messages read` line
# carries the timestamp to use here)
slack-cli messages thread #general 1700000000.000100

# Send messages
slack-cli send #general "Hello from the CLI"
slack-cli send @username "Hey there"
slack-cli send someone@company.com "Hey there"     # resolves by email (fastest)
slack-cli send @ana,@ben,carol@co.com "lunch?"     # group DM (up to 8 people)

# Reply inside a thread (anyone's, not just your own); --broadcast also
# posts the reply to the channel
slack-cli send #general "fixed it" -t 1700000000.000100 --broadcast

# Don't know their @handle? Resolve by full name via Jira (see below)
slack-cli send --name "Doe, Jane" "got a minute?"

# Attach files (-f is repeatable; the message is an optional caption)
slack-cli send #general "see attached" -f report.pdf
slack-cli send @ana -f a.png -f b.png

# List users
slack-cli users list
slack-cli users info @username
slack-cli users search ryan       # partial match on name, display name, or email
```

## Jira-backed Name Lookup (optional)

This is a plugin-style fallback, not a dependency — it only helps at orgs
where a person's Jira username is also their Slack username. Everywhere
else it's inert: skipped silently if Jira isn't configured. Connect it once
and `send --name`/`@name` lookups can resolve a person's full name
automatically when the Slack directory doesn't have an exact match:

```bash
slack-cli jira config                  # paste your Jira base URL + a Personal Access Token
slack-cli jira lookup "Doe, Jane"      # sanity-check the username it resolves to
slack-cli send --name "Doe, Jane" "got a minute?"
```

Under the hood, this calls Jira's user picker (the same endpoint behind
Jira's own @-mention autocomplete — no admin permissions needed) to turn the
name into a Jira username, then matches that username against the cached
Slack directory. Currently targets Jira Server/Data Center (Personal Access
Token auth); Jira Cloud isn't supported since it doesn't expose usernames
via its API.

## Performance & Caching

Resolving a `@user` or `#channel` requires the workspace directory, which on
large workspaces is thousands of paginated, rate-limited API calls. To keep
repeated sends fast, the directory is cached per workspace under
`~/.slack-cli/cache/` for 10 minutes — so the first send pays the cost once and
subsequent sends are effectively instant.

```bash
slack-cli --refresh channels list   # bypass the cache and fetch live
slack-cli cache clear               # delete all cached directories
```

Fastest of all: send by **email**, which uses a single direct lookup and skips
the directory scan entirely (`slack-cli send someone@company.com "hi"`).

## Multi-Workspace Support

Add multiple workspaces by running `auth login` for each:

```bash
slack-cli auth login    # adds first workspace
slack-cli auth login    # adds second workspace
```

Manage workspaces:
```bash
slack-cli auth list                 # show all workspaces (* = default)
slack-cli auth switch mycompany     # change default workspace
```

Use `-w` to target a specific workspace:
```bash
slack-cli -w mycompany channels list
slack-cli -w mycompany send #general "Hello"
```

## Features

- **No bot or Slack app required** — uses your browser session
- **Auto token extraction** — paste one cookie, token is fetched automatically
- **Multi-workspace** — switch between multiple Slack workspaces
- **Read & send messages** — channels, DMs, and thread replies
- **List channels & users** — browse your workspace, including starred/favorite channels
- **Jira-backed name lookup (optional)** — resolve a person's name to their Jira username via Jira when the Slack directory doesn't have an exact match, at orgs where that's also their Slack username
- **Pure Go** — single binary, no external dependencies

## Security

- Credentials stored in `~/.slack-cli/config.json` with `0600` permissions
- Tokens are scoped to your user account — same access as your browser session
- No data is sent anywhere except Slack's API servers

## Note

This tool uses Slack's internal web client API, which is not officially supported for third-party use. Tokens may expire when you log out of the browser. Re-run `slack-cli auth login` to refresh them.
