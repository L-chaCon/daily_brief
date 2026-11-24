# brief

A daily brief: collects what's waiting on you (GitHub, Jira; calendar planned), asks Claude to pick what
matters, and writes one calm HTML page for the day.

```
brief collect → raw.json → brief think → brief.json → brief render → brief.html
```

## Dependencies

| Tool | Why | Install |
|---|---|---|
| Go 1.27+ | build | `brew install go` |
| [`gh`](https://cli.github.com) | GitHub source (uses gh's login, no token to manage) | `brew install gh && gh auth login` |
| [`icalBuddy`](https://hasseg.org/icalBuddy/) | Calendar source (**planned**, not wired up yet): reads Calendar.app, so recurring events are already expanded | `brew install ical-buddy` |
| [`acli`](https://developer.atlassian.com/cloud/acli/) | Jira source (acli owns the login, like gh for GitHub) | `brew tap atlassian/homebrew-acli && brew install acli` |
| [`claude`](https://docs.claude.com/en/docs/claude-code) | think stage (`claude -p`) | see Claude Code docs, then log in |
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
```

## Installation

```bash
go build -o brief .
```

## Usage

```bash
brief run                      # collect, think, render, open today's brief
brief run --date 2026-11-24    # re-run a day
brief run --no-think           # skip the LLM: deterministic degraded page
brief collect | think | render | open [--date …]
```

Run folders: `~/.local/share/brief/YYYY-MM-DD/` (`raw.json`, `prompt.txt`, `claude.json`, `brief.json`, `brief.html`).
Config (optional): `~/.config/brief/config.json`.
