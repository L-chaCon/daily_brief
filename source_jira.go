package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Jira via `acli` (Atlassian's CLI): acli owns the OAuth login, like gh does for GitHub, so brief
// stores no token. acli's search only returns a few fields (no `updated`), so fetch is two steps:
// one search per query for the keys, then `workitem view` per key (all fields), a few at a time.
var jiraQueries = []struct{ kind, jql string }{
	{"issue_assigned", "assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC"},
	{"issue_watching_updated", "watcher = currentUser() AND assignee != currentUser() AND updated >= -1d ORDER BY updated DESC"},
}

const (
	jiraMaxItems    = 20
	jiraConcurrency = 4
	jiraViewFields  = "summary,status,priority,updated,project,reporter,assignee,issuetype,description"
)

// jiraFetched is what fetch hands to parse: the site (for browse links) and one view body per issue.
type jiraFetched struct {
	Site   string
	Issues []jiraRawIssue
}

type jiraRawIssue struct {
	Kind string
	Body []byte
}

func collectJira(ctx context.Context, env Env) ([]Item, error) {
	f, err := fetchJira(ctx, env)
	if err != nil {
		return nil, err
	}
	return parseJira(f, env)
}

func fetchJira(ctx context.Context, env Env) (jiraFetched, error) {
	var f jiraFetched
	status, err := env.Run(ctx, "acli", "jira", "auth", "status")
	if err != nil {
		return f, jiraErr(err)
	}
	f.Site = jiraSite(string(status))
	if f.Site == "" {
		return f, errors.New("jira: acli is not logged in. Run: acli jira auth login --web")
	}

	type job struct{ kind, key string }
	var jobs []job
	seen := map[string]bool{}
	for _, q := range jiraQueries {
		out, err := env.Run(ctx, "acli", "jira", "workitem", "search", "--jql", q.jql,
			"--fields", "key,summary", "--limit", fmt.Sprint(jiraMaxItems), "--json")
		if err != nil {
			return f, jiraErr(err)
		}
		var hits []struct{ Key string }
		if err := json.Unmarshal(out, &hits); err != nil {
			return f, fmt.Errorf("jira: decoding search: %w", err)
		}
		for _, h := range hits {
			if !seen[h.Key] && len(jobs) < jiraMaxItems { // assigned wins over watching
				seen[h.Key] = true
				jobs = append(jobs, job{q.kind, h.Key})
			}
		}
	}

	// Views run a few at a time: ~1s each, so 20 sequential views would blow the timeout.
	// Each goroutine writes only its own slot, so no mutex is needed.
	f.Issues = make([]jiraRawIssue, len(jobs))
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, jiraConcurrency)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := env.Run(ctx, "acli", "jira", "workitem", "view", j.key, "--fields", jiraViewFields, "--json")
			f.Issues[i], errs[i] = jiraRawIssue{Kind: j.kind, Body: out}, err
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return f, jiraErr(err)
	}
	return f, nil
}

func jiraErr(err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errors.New("jira: acli not found on PATH. Install: brew tap atlassian/homebrew-acli && brew install acli")
	case strings.Contains(strings.ToLower(err.Error()), "auth"):
		return errors.New("jira: acli is not logged in. Run: acli jira auth login --web")
	}
	return fmt.Errorf("jira: %w", err)
}

var jiraSiteRe = regexp.MustCompile(`(?m)Site:\s*(\S+)`)

func jiraSite(status string) string {
	if m := jiraSiteRe.FindStringSubmatch(status); m != nil {
		return strings.TrimPrefix(strings.TrimPrefix(m[1], "https://"), "http://")
	}
	return ""
}

type jiraIssue struct {
	Key    string
	Fields struct {
		Summary     string
		Updated     string // "2026-11-23T10:00:00.000+0000": not RFC 3339 (no colon in the offset)
		Description *adfNode
		Status      struct {
			Name           string
			StatusCategory struct{ Key string } // new | indeterminate | done
		}
		Priority  *struct{ Name string }
		Project   struct{ Key string }
		IssueType struct{ Name string }
		Reporter  *struct{ DisplayName string }
		Assignee  *struct{ DisplayName string }
	}
}

const jiraTime = "2006-01-02T15:04:05.000-0700"

// parseJira is pure: fetched bodies in, Items out.
func parseJira(f jiraFetched, env Env) ([]Item, error) {
	var items []Item
	for _, raw := range f.Issues {
		var is jiraIssue
		if err := json.Unmarshal(raw.Body, &is); err != nil {
			return nil, fmt.Errorf("jira: decoding issue: %w", err)
		}
		updated, err := time.Parse(jiraTime, is.Fields.Updated)
		if err != nil {
			return nil, fmt.Errorf("jira: %s: updated %q: %w", is.Key, is.Fields.Updated, err)
		}
		var who []string
		for _, p := range []*struct{ DisplayName string }{is.Fields.Assignee, is.Fields.Reporter} {
			if p != nil && p.DisplayName != "" && !slices.Contains(who, p.DisplayName) {
				who = append(who, p.DisplayName)
			}
		}
		facts := []string{"status: " + is.Fields.Status.Name, "type: " + is.Fields.IssueType.Name, "project: " + is.Fields.Project.Key}
		if p := is.Fields.Priority; p != nil && p.Name != "" {
			facts = append(facts, "priority: "+p.Name)
		}
		facts = append(facts, age(env.Now, updated))
		items = append(items, Item{
			ID: "jira:issue:" + is.Key, Source: "jira", Kind: raw.Kind,
			Title: oneLine(is.Key + " " + is.Fields.Summary), URL: "https://" + f.Site + "/browse/" + is.Key,
			Who: who, When: updated.In(env.Loc), Body: plainBody(adfText(is.Fields.Description)), Facts: compact(facts),
		})
	}
	return items, nil
}

// adfNode is one node of the Atlassian Document Format tree (Jira v3 rich text).
type adfNode struct {
	Type    string
	Text    string
	Content []adfNode
}

var adfBlocks = map[string]bool{"paragraph": true, "heading": true, "codeBlock": true, "blockquote": true, "tableRow": true}

// adfText flattens the tree to plain text: text nodes concatenated, a newline after each block.
// Links keep their visible text only (the URL lives in marks, which we ignore on purpose).
func adfText(n *adfNode) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(n adfNode)
	walk = func(n adfNode) {
		switch n.Type {
		case "text":
			b.WriteString(n.Text)
		case "hardBreak":
			b.WriteString("\n")
		case "mention", "emoji":
			// attrs only; skip
		}
		for _, c := range n.Content {
			walk(c)
		}
		if adfBlocks[n.Type] {
			b.WriteString("\n")
		}
	}
	walk(*n)
	return b.String()
}
