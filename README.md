# brief

A daily brief: collects what's waiting on you (GitHub, Jira, Slack DMs, Google Calendar), asks Claude to pick what
matters, and writes one calm HTML page for the day.

```
brief collect → raw.json → brief think → brief.json → brief render → brief.html
```

## Dependencies

| Tool | Why | Install |
|---|---|---|
| Go 1.27+ | build | `brew install go` |
| [`gh`](https://cli.github.com) | GitHub source (uses gh's login, no token to manage) | `brew install gh && gh auth login` |
| [`icalBuddy`](https://hasseg.org/icalBuddy/) | **Planned** local Calendar transport (today Calendar comes from the claude.ai Google Calendar connector) | `brew install ical-buddy` |
| [`acli`](https://developer.atlassian.com/cloud/acli/) | Jira source (acli owns the login, like gh for GitHub) | `brew tap atlassian/homebrew-acli && brew install acli` |
| [`claude`](https://docs.claude.com/en/docs/claude-code) | think stage (`claude -p`); Slack and Calendar sources through the claude.ai Slack / Google Calendar connectors (no app, no token) | see Claude Code docs, then log in with the account that has the Slack connector |
| `gitleaks` | pre-commit secret scan | `brew install gitleaks` |

macOS only: Calendar access is granted per app. `icalBuddy calendars` must list your calendars from
the terminal (or launchd job) that runs `brief`; some terminals can't request the permission yet.

## Connecting accounts

No tokens are stored by `brief`: each source reuses a tool that already handles its own login.

```bash
gh auth login                      # GitHub
acli jira auth login --web         # Jira: browser (OAuth) login, acli stores and refreshes it
acli jira auth status              # check it
claude                             # think stage: log in once with /login
                                   # Slack/Calendar: same login; that account must have the claude.ai Slack and Google Calendar connectors.
                                   # Set "claude_config_dir" in config.json to that account.
```

## Installation

```bash
brew install L-chaCon/tap/brief    # Homebrew: builds the latest release from source
# or, from a clone:
go build -o brief .
```

Releasing: tag `vX.Y.Z` and push the tag, then update `url` and `sha256` in
[`L-chaCon/homebrew-tap`](https://github.com/L-chaCon/homebrew-tap) `Formula/brief.rb`.

## Usage

```bash
brief run                      # collect, think, render, open today's brief
brief run --date 2026-11-24    # re-run a day
brief run --no-think           # skip the LLM: deterministic degraded page
brief collect | think | render | open [--date …]
```

Run folders: `~/.local/share/brief/YYYY-MM-DD/` (`raw.json`, `slack.claude.jsonl`, `calendar.claude.jsonl`, `prompt.txt`, `claude.json`, `brief.json`, `brief.html`).
Config (optional): `~/.config/brief/config.json`.
