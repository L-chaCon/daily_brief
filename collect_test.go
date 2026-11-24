package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
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
		{"a", func(context.Context, Env) ([]Item, error) { panic("boom") }},
		{"b", good},
		{"c", func(context.Context, Env) ([]Item, error) { return nil, errors.New("c: 401. Run: brief connect c") }},
		{"off", good},
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
