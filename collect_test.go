package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEnv(t *testing.T) Env {
	t.Helper()
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return Env{
		Now: time.Date(2026, 10, 14, 8, 0, 0, 0, london), Loc: london, DataDir: t.TempDir(),
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			t.Errorf("unexpected exec: %s %v", name, args)
			return nil, errors.New("no exec in tests")
		},
	}
}

func TestParseGitHub(t *testing.T) {
	env := testEnv(t)
	body, err := os.ReadFile("testdata/github/response.json")
	if err != nil {
		t.Fatal(err)
	}
	items, err := parseGitHub(body, env)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Item{}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			t.Error(err)
		}
		byID[it.ID] = it
	}
	if len(items) != 4 {
		t.Errorf("got %d items, want 4 (the draft review request is skipped)", len(items))
	}

	tests := []struct {
		id, kind  string
		wantFacts []string
	}{
		{"github:pr:acme/ops-app#215", "pr_review_requested", []string{"review: REVIEW_REQUIRED", "checks: success", "updated: today", "jira: OPS-1460"}},
		{"github:pr:acme/ops-app#210", "pr_mine_ready", []string{"approvals: 2", "mergeable: yes", "jira: OPS-1450", "updated: 2d ago"}},
		{"github:pr:acme/infra#12", "pr_mine_open", []string{"review: CHANGES_REQUESTED", "checks: none", "mergeable: no (conflicts)"}},
		{"github:issue:acme/ops-app#7", "issue_assigned", []string{"updated: today"}},
	}
	for _, tt := range tests {
		it, ok := byID[tt.id]
		if !ok {
			t.Errorf("missing %s", tt.id)
			continue
		}
		if it.Kind != tt.kind {
			t.Errorf("%s: kind %q, want %q", tt.id, it.Kind, tt.kind)
		}
		for _, f := range tt.wantFacts {
			if !strings.Contains(strings.Join(it.Facts, "|"), f) {
				t.Errorf("%s: facts %v missing %q", tt.id, it.Facts, f)
			}
		}
	}
	pr210 := byID["github:pr:acme/ops-app#210"]
	if strings.Join(pr210.Who, ",") != "me-example,alex-example,sam-example" {
		t.Errorf("who = %v, want author then approvers", pr210.Who)
	}
	if _, off := pr210.When.Zone(); off != 3600 {
		t.Errorf("When should be in env.Loc (BST), offset %d", off)
	}
	if got := byID["github:pr:acme/ops-app#215"].Body; got != "Adds a bounded retry around the printer call.\n\nSee notes." {
		t.Errorf("body not flattened: %q", got)
	}
}

func TestParseGitHubErrors(t *testing.T) {
	env := testEnv(t)
	if _, err := parseGitHub([]byte(`{"errors":[{"message":"Bad credentials"}]}`), env); err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("graphql error not surfaced: %v", err)
	}
	if _, err := parseGitHub([]byte(`not json`), env); err == nil {
		t.Error("want decode error")
	}
}

func TestFetchGitHubWithoutGh(t *testing.T) {
	env := testEnv(t)
	env.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, &exec.Error{Name: "gh", Err: exec.ErrNotFound}
	}
	_, err := fetchGitHub(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "brew install gh") {
		t.Errorf("want an actionable error, got %v", err)
	}
}

func TestCollect(t *testing.T) {
	env := testEnv(t)
	at := env.Now.Add(-time.Hour)
	good := func(context.Context, Env) ([]Item, error) {
		return []Item{
			{ID: "b:2", Source: "b", Kind: "k", Title: "later", When: at.Add(time.Minute)},
			{ID: "b:1", Source: "b", Kind: "k", Title: "earlier", When: at},
			{ID: "b:1", Source: "b", Kind: "k", Title: "duplicate", When: at},
			{ID: "wrong-prefix", Source: "b", Kind: "k", Title: "invalid", When: at},
		}, nil
	}
	table := []source{
		{"a", func(context.Context, Env) ([]Item, error) { panic("boom") }, 0},
		{"b", good, 0},
		{"c", func(context.Context, Env) ([]Item, error) { return nil, errors.New("c: 401. Run: brief connect c") }, 0},
		{"off", good, 0},
	}
	env.Config.Disabled = []string{"off"}
	raw := collectAll(context.Background(), env, civilDate{2026, 10, 14}, table)

	if len(raw.Sources) != 3 {
		t.Fatalf("sources = %+v, want a, b, c (off is disabled)", raw.Sources)
	}
	a, b, c := raw.Sources[0], raw.Sources[1], raw.Sources[2]
	if a.OK || !strings.Contains(a.Error, "panic: boom") {
		t.Errorf("a panicking collector must become a status, got %+v", a)
	}
	if !b.OK || b.Count != 2 || !strings.Contains(b.Error, "2 invalid or duplicate items dropped") {
		t.Errorf("b = %+v", b)
	}
	if c.OK || !strings.Contains(c.Error, "brief connect c") {
		t.Errorf("c = %+v", c)
	}
	if len(raw.Items) != 2 || raw.Items[0].ID != "b:1" || raw.Items[1].ID != "b:2" {
		t.Errorf("items must be deduped, validated and sorted by When: %+v", raw.Items)
	}
	if raw.Timezone != "Europe/London" || raw.Date.String() != "2026-10-14" {
		t.Errorf("header = %q %s", raw.Timezone, raw.Date)
	}
}

func TestParseJira(t *testing.T) {
	env := testEnv(t)
	b1, _ := os.ReadFile("testdata/jira/ops-1452.json")
	b2, _ := os.ReadFile("testdata/jira/ops-1460.json")
	items, err := parseJira(jiraFetched{Site: "acme.atlassian.net", Issues: []jiraRawIssue{
		{"issue_assigned", b1}, {"issue_watching_updated", b2},
	}}, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			t.Error(err)
		}
	}
	a := items[0]
	if a.ID != "jira:issue:OPS-1452" || a.Kind != "issue_assigned" || a.URL != "https://acme.atlassian.net/browse/OPS-1452" {
		t.Errorf("item = %+v", a)
	}
	if a.Title != "OPS-1452 Delivery settings page shows the wrong cut-off time" {
		t.Errorf("title = %q (the key goes in the title: it's how people talk about tickets)", a.Title)
	}
	if got := strings.Join(a.Facts, "|"); got != "status: In Progress|type: Bug|project: OPS|priority: High|updated: today" {
		t.Errorf("facts = %s", got)
	}
	if _, off := a.When.Zone(); off != 3600 || a.When.Hour() != 17 {
		t.Errorf("When = %v, want 17:40 BST (Jira's +0000 converted to env.Loc)", a.When)
	}
	if strings.Join(a.Who, ",") != "Me Example,Riley Example" {
		t.Errorf("who = %v", a.Who)
	}
	b := items[1]
	if b.Body != "" || len(b.Who) != 1 || strings.Contains(strings.Join(b.Facts, "|"), "priority") {
		t.Errorf("nulls should mean absent, got %+v", b)
	}
}

func TestADFText(t *testing.T) {
	b, _ := os.ReadFile("testdata/jira/ops-1452.json")
	var is jiraIssue
	if err := json.Unmarshal(b, &is); err != nil {
		t.Fatal(err)
	}
	want := "Steps\nOpen settings, see the cut-off\nRunbook: https://example.com/runbook\nUK only\n"
	if got := adfText(is.Fields.Description); got != want {
		t.Errorf("adfText =\n%q\nwant\n%q", got, want)
	}
	if adfText(nil) != "" {
		t.Error("nil description")
	}
}

func TestFetchJira(t *testing.T) {
	env := testEnv(t)
	b1, _ := os.ReadFile("testdata/jira/ops-1452.json")
	var calls []string
	var mu sync.Mutex
	env.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, strings.Join(args[:min(3, len(args))], " "))
		mu.Unlock()
		switch {
		case args[1] == "auth":
			return []byte("✓ Authenticated\n  Site: acme.atlassian.net\n  Authentication Type: oauth\n"), nil
		case args[2] == "search" && strings.Contains(args[4], "assignee = currentUser()"):
			return []byte(`[{"key":"OPS-1452"}]`), nil
		case args[2] == "search":
			return []byte(`[{"key":"OPS-1452"}]`), nil // watched AND assigned: assigned wins, no duplicate view
		case args[2] == "view":
			return b1, nil
		}
		return nil, fmt.Errorf("unexpected %v", args)
	}
	f, err := fetchJira(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if f.Site != "acme.atlassian.net" || len(f.Issues) != 1 || f.Issues[0].Kind != "issue_assigned" {
		t.Errorf("fetched = site %q, %d issues", f.Site, len(f.Issues))
	}
	if len(calls) != 4 {
		t.Errorf("calls = %v, want auth + 2 searches + 1 view", calls)
	}

	env.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, &exec.Error{Name: "acli", Err: exec.ErrNotFound}
	}
	if _, err := fetchJira(context.Background(), env); err == nil || !strings.Contains(err.Error(), "brew install acli") {
		t.Errorf("want install hint, got %v", err)
	}
}
