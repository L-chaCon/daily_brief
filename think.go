package main

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

//go:embed assets/prompt.md
var promptTemplate string

//go:embed assets/brief.schema.json
var briefSchema []byte

// Thinker is the LLM backend as a function value: (system, user) -> JSON matching briefSchema.
// The real one wraps `claude -p`; tests pass a fake. No interface, no mock library.
type Thinker func(ctx context.Context, system, user string) (json.RawMessage, error)

const (
	maxPromptChars = 60_000
	maxWorkItems   = 40
	maxReading     = 15
	maxPromptBody  = 400
	thinkTimeout   = 3 * time.Minute
)

// kindPriority orders work items when the cap bites: someone waiting on you comes first.
var kindPriority = map[string]int{
	"pr_review_requested": 0, "pr_mine_ready": 1, "issue_assigned": 2, "pr_mine_open": 3,
	"issue_mentioned": 4, "issue_watching_updated": 5,
}

// promptItem is what the LLM sees of an Item: no URL (it can't copy what it never saw),
// times preformatted (no time zone maths), body shortened.
type promptItem struct {
	ID     string   `json:"id"`
	Source string   `json:"source"`
	Kind   string   `json:"kind"`
	Title  string   `json:"title"`
	Day    string   `json:"day,omitempty"`  // events: "today" | "tomorrow"
	When   string   `json:"when,omitempty"` // events: "14:00–14:15" | "all day"
	Who    []string `json:"who,omitempty"`
	Facts  []string `json:"facts,omitempty"`
	Body   string   `json:"body,omitempty"`
}

// buildPrompt is pure: raw + config in, (system, user) out. Golden-tested.
func buildPrompt(raw Raw, cfg Config) (system, user string, err error) {
	loc, err := time.LoadLocation(raw.Timezone)
	if err != nil {
		return "", "", fmt.Errorf("think: raw.json timezone %q: %w", raw.Timezone, err)
	}
	day := raw.Date.In(loc)

	var work, cal, reading []Item
	invalid := 0
	for _, it := range raw.Items {
		switch {
		case slices.Contains(cfg.NeverSend, it.Source):
			continue // rendered on the page, never sent to the LLM
		case it.Validate() != nil:
			invalid++ // e.g. an email address in facts must not reach the LLM
		case it.Source == "calendar":
			if w := it.When.In(loc); !w.Before(day) && w.Before(day.AddDate(0, 0, 2)) {
				cal = append(cal, it)
			}
		case isReading(it.Source):
			reading = append(reading, it)
		default:
			work = append(work, it)
		}
	}

	slices.SortStableFunc(work, func(a, b Item) int {
		return cmp.Or(cmp.Compare(priority(a.Kind), priority(b.Kind)), a.When.Compare(b.When))
	})
	var notes []string
	if n := len(work) - maxWorkItems; n > 0 {
		work = work[:maxWorkItems]
		notes = append(notes, fmt.Sprintf("NOTE: %d lower-priority work items omitted", n))
	}
	reading = pickReading(reading, cfg.Interests)
	if invalid > 0 {
		notes = append(notes, fmt.Sprintf("NOTE: %d invalid items omitted", invalid))
	}

	var u strings.Builder
	u.WriteString("SOURCES:")
	for i, s := range raw.Sources {
		sep := " "
		if i > 0 {
			sep = " · "
		}
		if s.OK {
			fmt.Fprintf(&u, "%s%s ok (%d)", sep, s.Name, s.Count)
		} else {
			fmt.Fprintf(&u, "%s%s FAILED", sep, s.Name)
		}
	}
	u.WriteString("\n")
	for _, n := range notes {
		u.WriteString(n + "\n")
	}
	u.WriteString("ITEMS:\n")
	for _, group := range [][]Item{work, cal, reading} {
		for _, it := range group {
			line, err := json.Marshal(toPromptItem(it, day, loc))
			if err != nil {
				return "", "", err
			}
			u.Write(line)
			u.WriteString("\n")
		}
	}

	interests := strings.Join(cfg.Interests, ", ")
	if interests == "" {
		interests = "software engineering"
	}
	system = strings.NewReplacer(
		"{{interests}}", interests,
		"{{weekday}}", day.Weekday().String(),
		"{{date}}", raw.Date.String(),
		"{{timezone}}", raw.Timezone,
	).Replace(promptTemplate)
	user = u.String()
	if n := len(system) + len(user); n > maxPromptChars {
		return "", "", fmt.Errorf("think: prompt is %d chars, max %d (a cap upstream is wrong)", n, maxPromptChars)
	}
	return system, user, nil
}

func priority(kind string) int {
	if p, ok := kindPriority[kind]; ok {
		return p
	}
	return len(kindPriority)
}

func toPromptItem(it Item, day time.Time, loc *time.Location) promptItem {
	p := promptItem{ID: it.ID, Source: it.Source, Kind: it.Kind, Title: it.Title, Who: it.Who, Facts: it.Facts,
		Body: truncate(it.Body, maxPromptBody)}
	if it.Source == "calendar" {
		w := it.When.In(loc)
		p.Day = "today"
		if !w.Before(day.AddDate(0, 0, 1)) {
			p.Day = "tomorrow"
		}
		switch {
		case it.Kind == "event_all_day":
			p.When = "all day"
		case it.End != nil:
			p.When = w.Format("15:04") + "–" + it.End.In(loc).Format("15:04")
		default:
			p.When = w.Format("15:04")
		}
	}
	return p
}

// pickReading keeps the reading items that best match the interests (keyword hits in
// title+body), newest first on ties, at most maxReading.
func pickReading(items []Item, interests []string) []Item {
	score := func(it Item) int {
		text, n := strings.ToLower(it.Title+" "+it.Body), 0
		for _, k := range interests {
			if strings.Contains(text, strings.ToLower(k)) {
				n++
			}
		}
		return n
	}
	slices.SortStableFunc(items, func(a, b Item) int {
		return cmp.Or(cmp.Compare(score(b), score(a)), b.When.Compare(a.When))
	})
	return items[:min(len(items), maxReading)]
}

// think asks once, and retries once with the list of problems if the answer fails validation.
// CLI errors are not retried: that's a broken setup, not a bad answer.
func think(ctx context.Context, raw Raw, cfg Config, llm Thinker) (*Brief, error) {
	system, user, err := buildPrompt(raw, cfg)
	if err != nil {
		return nil, err
	}
	b, errs, err := ask(ctx, llm, system, user, raw)
	if err != nil {
		return nil, err
	}
	if len(errs) == 0 {
		return b, nil
	}
	retry := user + "\n\nYOUR PREVIOUS ANSWER WAS INVALID:\n- " + joinErrs(errs) + "\nReturn a corrected answer."
	b, errs, err = ask(ctx, llm, system, retry, raw)
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("think: invalid after retry:\n- %s", joinErrs(errs))
	}
	return b, nil
}

func ask(ctx context.Context, llm Thinker, system, user string, raw Raw) (*Brief, []error, error) {
	out, err := llm(ctx, system, user)
	if err != nil {
		return nil, nil, err
	}
	var b Brief
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return nil, []error{fmt.Errorf("decode: %w", err)}, nil
	}
	return &b, Validate(b, raw), nil
}

func joinErrs(errs []error) string {
	s := make([]string, len(errs))
	for i, e := range errs {
		s[i] = e.Error()
	}
	return strings.Join(s, "\n- ")
}

// claudeEnvelope is the subset of `claude -p --output-format json` we rely on.
type claudeEnvelope struct {
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	DurationMS       int64           `json:"duration_ms"`
	NumTurns         int             `json:"num_turns"`
}

func parseEnvelope(b []byte) (json.RawMessage, error) {
	var e claudeEnvelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("claude: bad envelope: %w", err)
	}
	if e.IsError || e.Subtype != "success" {
		msg, _, _ := strings.Cut(e.Result, "\n")
		return nil, fmt.Errorf("claude: %s: %s", e.Subtype, msg)
	}
	if len(e.StructuredOutput) == 0 || string(e.StructuredOutput) == "null" {
		return nil, errors.New("claude: no structured_output in the response")
	}
	return e.StructuredOutput, nil
}

// claudeThinker runs `claude -p` with no tools and the brief schema. Each raw envelope is saved
// to <dir>/claude.json (cost, timing, model) for later `jq` over the run folders.
func claudeThinker(env Env, dir string) Thinker {
	return func(ctx context.Context, system, user string) (json.RawMessage, error) {
		ctx, cancel := context.WithTimeout(ctx, thinkTimeout)
		defer cancel()
		model := cmp.Or(env.Config.Model, "sonnet")
		cmd := exec.CommandContext(ctx, "claude", "-p",
			"--model", model,
			"--tools", "", // no tools: text inside items can't make it do anything
			// Isolated from the user's Claude setup: no MCP servers, hooks, plugins, settings or skills.
			// Same result on every machine, and nothing else ever sees this prompt of work data.
			"--strict-mcp-config", "--setting-sources", "", "--disable-slash-commands",
			"--no-session-persistence",
			"--output-format", "json",
			"--json-schema", string(briefSchema),
			"--system-prompt", system)
		cmd.Stdin = strings.NewReader(user)
		if d := env.Config.ClaudeConfigDir; d != "" {
			cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+expandHome(d))
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if len(out) > 0 {
			_ = os.WriteFile(filepath.Join(dir, "claude.json"), out, 0o644)
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("claude: not found on PATH. Install Claude Code, or set PATH in the launchd plist")
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("claude: timeout after %s", thinkTimeout)
		}
		if err != nil && len(out) == 0 {
			msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
			return nil, fmt.Errorf("claude: %w: %s", err, msg)
		}
		return parseEnvelope(out)
	}
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// thinkDir runs think for one run folder and leaves exactly one of brief.json / think.error.txt,
// so render never shows a stale brief from an earlier run.
func thinkDir(ctx context.Context, env Env, dir string, llm Thinker, dryRun bool) error {
	var raw Raw
	if err := readJSON(filepath.Join(dir, "raw.json"), &raw); err != nil {
		return err
	}
	system, user, err := buildPrompt(raw, env.Config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(system+"\n---\n"+user), 0o644); err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	os.Remove(filepath.Join(dir, "brief.json"))
	os.Remove(filepath.Join(dir, "think.error.txt"))
	b, err := think(ctx, raw, env.Config, llm)
	if err != nil {
		_ = os.WriteFile(filepath.Join(dir, "think.error.txt"), []byte(err.Error()+"\n"), 0o644)
		return err
	}
	return writeJSONFile(filepath.Join(dir, "brief.json"), b)
}
