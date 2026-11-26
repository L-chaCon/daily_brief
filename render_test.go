package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// golden compares got with the file at path; -update rewrites it. Review `git diff testdata/` after.
func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run with -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch; run `go test -run %s -update` and review the diff", path, t.Name())
	}
}

func loadBasic(t *testing.T) (Raw, Brief) {
	t.Helper()
	var r Raw
	var b Brief
	loadJSON(t, basicDir+"/raw.json", &r)
	loadJSON(t, basicDir+"/brief.json", &b)
	return r, b
}

// basicRunDir is a run folder holding the basic fixture's raw.json and brief.json.
func basicRunDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"raw.json", "brief.json"} {
		b, err := os.ReadFile(filepath.Join(basicDir, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestJoin(t *testing.T) {
	raw, b := loadBasic(t)
	p := join(raw, &b, nil)

	if p.Weekday != "Wednesday" || p.Date != "2026-10-14" || p.GeneratedAt != "08:00" {
		t.Errorf("header = %q %q %q", p.Weekday, p.Date, p.GeneratedAt)
	}
	if p.Degraded {
		t.Error("valid brief should not be degraded")
	}
	if p.Focus == nil || p.Focus.ID != "focus:github:pr:acme/ops-app#210" || len(p.Focus.Links) != 1 {
		t.Errorf("focus = %+v", p.Focus)
	}
	if len(p.Todos) != 2 || p.Todos[0].Links[0].URL != "https://github.com/acme/ops-app/pull/210" {
		t.Errorf("todos = %+v", p.Todos)
	}
	if got := strings.Join(p.Todos[0].Who, ","); got != "Alex Example,Sam Example" {
		t.Errorf("todo who = %q", got)
	}
	// Schedule: deterministic skeleton, LLM note attached to the standup only.
	morning, afternoon, evening := p.Schedule[0], p.Schedule[1], p.Schedule[2]
	if len(morning.Events) != 1 || morning.Events[0].Start != "10:00" || morning.Events[0].End != "10:30" {
		t.Errorf("morning = %+v", morning)
	}
	if len(afternoon.Events) != 1 || afternoon.Events[0].Note == "" || afternoon.Events[0].PrepPrompt == "" {
		t.Errorf("afternoon = %+v", afternoon)
	}
	if len(evening.Events) != 0 {
		t.Errorf("evening = %+v", evening)
	}
	if len(p.Tomorrow) != 1 || p.Tomorrow[0].Title != "Sprint planning" {
		t.Errorf("tomorrow = %+v", p.Tomorrow)
	}
	if len(p.Reading) != 1 || p.Reading[0].Title != "Designing agentic loops" {
		t.Errorf("reading = %+v", p.Reading)
	}
}

func TestJoinDegraded(t *testing.T) {
	raw, _ := loadBasic(t)
	p := join(raw, nil, errors.New("claude: exit 1\nlong stack"))
	if !p.Degraded || p.DegradedWhy != "claude: exit 1" {
		t.Errorf("degraded = %v %q, want first line of the error", p.Degraded, p.DegradedWhy)
	}
	if p.Greeting != "2 meetings today, 3 work items open." {
		t.Errorf("greeting = %q", p.Greeting)
	}
	// Actionable kinds only (the Jira "watching" update is not a to-do), oldest first.
	var titles []string
	for _, c := range p.Todos {
		titles = append(titles, c.Title)
	}
	if got := strings.Join(titles, " | "); got != "Package count on packing screen | Retry label printing on timeout" {
		t.Errorf("degraded todos = %s", got)
	}
	if len(p.Schedule[0].Events)+len(p.Schedule[1].Events) != 2 {
		t.Error("the schedule must not depend on the LLM")
	}
}

// Decision D4 as code: every link on the page comes from some item in raw.
func TestJoinNeverInventsLinks(t *testing.T) {
	raw, b := loadBasic(t)
	urls := map[string]bool{}
	for _, it := range raw.Items {
		urls[it.URL] = true
	}
	for _, p := range []Page{join(raw, &b, nil), join(raw, nil, nil)} {
		cards := append(append(append([]Card{}, p.Todos...), p.Updates...), p.Reading...)
		if p.Focus != nil {
			cards = append(cards, *p.Focus)
		}
		for _, c := range cards {
			for _, l := range c.Links {
				if !urls[l.URL] {
					t.Errorf("card %q links to %q, which is not in raw", c.ID, l.URL)
				}
			}
		}
	}
}

func TestRenderGolden(t *testing.T) {
	raw, b := loadBasic(t)
	html, err := renderHTML(join(raw, &b, nil))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, basicDir+"/golden.html", html)
}

func TestRenderDegraded(t *testing.T) {
	raw, _ := loadBasic(t)
	html, err := renderHTML(join(raw, nil, errors.New("think skipped (--no-think)")))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"degraded mode: think skipped (--no-think)", "brief think --date 2026-10-14", "mail: Mail.app is not running"} {
		if !bytes.Contains(html, []byte(want)) {
			t.Errorf("degraded page missing %q", want)
		}
	}
	golden(t, basicDir+"/golden_degraded.html", html)
}

func TestRenderEscaping(t *testing.T) {
	const evil = `<script>alert(1)</script>`
	p := Page{
		Date: "2026-10-14", Weekday: "Wednesday", Greeting: evil, Degraded: true, DegradedWhy: evil,
		Focus:    &Card{ID: evil, Title: evil, Body: evil, Prompt: evil, Who: []string{evil}, Links: []Link{{Text: evil, URL: "javascript:alert(1)"}}},
		Todos:    []Card{{ID: `"><img src=x onerror=alert(1)>`, Title: evil, Label: evil}},
		Schedule: []DayPart{{Name: "Morning", Events: []Event{{Title: evil, Note: evil, PrepPrompt: evil}}}},
		Sources:  []SourceStatus{{Name: evil, Error: evil}},
	}
	html, err := renderHTML(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(html, []byte(evil)) || bytes.Contains(html, []byte("<img")) {
		t.Error("unescaped markup in output")
	}
	if bytes.Contains(html, []byte("javascript:")) {
		t.Error("javascript: URL survived")
	}
	if !bytes.Contains(html, []byte("&lt;script&gt;")) {
		t.Error("expected escaped &lt;script&gt;")
	}
}

// template.HTML (and friends) switch off escaping. Data must never be converted to them.
func TestNoTrustedTemplateTypes(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, bad := range []string{"template.HTML(", "template.JS(", "template.URL(", "template.CSS(", "template.HTMLAttr("} {
			if bytes.Contains(src, []byte(bad)) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}

func TestRenderCommand(t *testing.T) {
	dir := basicRunDir(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"render", "--dir", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	html, _ := os.ReadFile(filepath.Join(dir, "brief.html"))
	if bytes.Contains(html, []byte("degraded mode")) {
		t.Error("valid brief.json rendered degraded")
	}

	// An invalid brief.json degrades the page instead of failing the command.
	os.WriteFile(filepath.Join(dir, "brief.json"), []byte(`{"schema": 2}`), 0o644)
	if code := run([]string{"render", "--dir", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	html, _ = os.ReadFile(filepath.Join(dir, "brief.html"))
	if !bytes.Contains(html, []byte("failed validation")) {
		t.Error("invalid brief.json should show in the degraded banner")
	}

	// Missing raw.json is a real error.
	if code := run([]string{"render", "--dir", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Errorf("missing raw.json: exit %d, want 1", code)
	}
}

func TestRenderShowsClaudeAccount(t *testing.T) {
	dir := basicRunDir(t)
	writeJSONFile(filepath.Join(dir, "think.json"), thinkMeta{Account: "~/.claude-work", Model: "sonnet"})
	if _, err := renderDir(dir, false); err != nil {
		t.Fatal(err)
	}
	html, _ := os.ReadFile(filepath.Join(dir, "brief.html"))
	if !bytes.Contains(html, []byte("written by sonnet · ~/.claude-work")) {
		t.Error("footer must say which Claude account wrote the brief")
	}
}
