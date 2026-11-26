# brief

A daily brief: collects what's waiting on you (GitHub, Jira, Slack DMs, Google Calendar), asks Claude to pick what
matters, and writes one calm HTML page for the day.

```
brief collect → raw.json → brief think → brief.json → brief render → brief.html
```

## Install

```bash
brew install L-chaCon/tap/brief
```

`brief` stores no tokens: each source reuses a tool that already owns its login. Install the ones you use:

| Tool | Used for | Install |
|---|---|---|
| [`gh`](https://cli.github.com) | GitHub | `brew install gh` |
| [`acli`](https://developer.atlassian.com/cloud/acli/) | Jira | `brew install atlassian/acli/acli` |
| [Claude Code](https://docs.claude.com/en/docs/claude-code) | picking what matters (`claude -p`); Slack and Calendar through the claude.ai Slack / Google Calendar connectors | `brew install --cask claude-code` |

A source that isn't set up shows its fix on the page; the rest of the brief still works.

## Connecting accounts

```bash
gh auth login                      # GitHub
acli jira auth login --web         # Jira: browser (OAuth) login, acli stores and refreshes it
claude                             # then /login, with the account that has the claude.ai Slack and Google Calendar connectors
```

If that Claude account isn't your default one, point `brief` at it in `~/.config/brief/config.json`:

```json
{ "claude_config_dir": "~/.claude-work" }
```

## Usage

```bash
brief run                      # collect, think, render, open today's brief
brief run --date 2026-11-24    # re-run a day
brief run --no-think           # skip the LLM: deterministic degraded page
brief collect | think | render | open [--date …]
```

Run folders: `~/.local/share/brief/YYYY-MM-DD/` (`raw.json`, `slack.claude.jsonl`, `calendar.claude.jsonl`, `prompt.txt`, `claude.json`, `brief.json`, `brief.html`).
Config (optional): `~/.config/brief/config.json`.
