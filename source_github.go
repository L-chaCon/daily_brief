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
	"time"
	"unicode/utf8"
)

// GitHub via `gh api graphql` (doc 03 §1): gh already holds the token, so there's no auth to build.
// One request, three aliased searches. user-review-requested (not review-requested) excludes team requests.
const githubQuery = `
fragment pr on PullRequest {
  number title url updatedAt isDraft reviewDecision mergeable headRefName body
  author { login }
  repository { nameWithOwner }
  reviews(states: APPROVED, first: 10) { totalCount nodes { author { login } } }
  commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
}
fragment issue on Issue { number title url updatedAt body author { login } repository { nameWithOwner } }
{
  review: search(query: "is:pr is:open user-review-requested:@me archived:false", type: ISSUE, first: 30) { nodes { ...pr } }
  mine:   search(query: "is:pr is:open author:@me archived:false", type: ISSUE, first: 30) { nodes { ...pr } }
  issues: search(query: "is:issue is:open assignee:@me archived:false", type: ISSUE, first: 30) { nodes { ...issue } }
}`

func collectGitHub(ctx context.Context, env Env) ([]Item, error) {
	b, err := fetchGitHub(ctx, env)
	if err != nil {
		return nil, err
	}
	return parseGitHub(b, env)
}

func fetchGitHub(ctx context.Context, env Env) ([]byte, error) {
	out, err := env.Run(ctx, "gh", "api", "graphql", "-f", "query="+githubQuery)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, errors.New("github: gh not found on PATH. Install: brew install gh && gh auth login")
	case err != nil && strings.Contains(err.Error(), "auth login"):
		return nil, errors.New("github: gh is not logged in. Run: gh auth login")
	case err != nil:
		return nil, fmt.Errorf("github: %w", err)
	}
	return out, nil
}

type ghResponse struct {
	Data struct {
		Review, Mine, Issues struct{ Nodes []ghNode }
	}
	Errors []struct{ Message string }
}

type ghNode struct {
	Number         int
	Title, URL     string
	UpdatedAt      time.Time
	IsDraft        bool
	ReviewDecision string // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | "" (no review policy)
	Mergeable      string // MERGEABLE | CONFLICTING | UNKNOWN
	HeadRefName    string
	Body           string
	Author         struct{ Login string }
	Repository     struct{ NameWithOwner string }
	Reviews        struct {
		TotalCount int
		Nodes      []struct{ Author struct{ Login string } }
	}
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct{ State string }
			}
		}
	}
}

// parseGitHub is pure: bytes in, Items out. All the mapping logic lives here, tested with fixtures.
func parseGitHub(body []byte, env Env) ([]Item, error) {
	var r ghResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("github: decoding response: %w", err)
	}
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("github: %s", r.Errors[0].Message)
	}
	var items []Item
	for _, n := range r.Data.Review.Nodes {
		if n.IsDraft { // a draft isn't really asking for your review yet
			continue
		}
		it := ghPR(n, env)
		it.Kind = "pr_review_requested"
		items = append(items, it)
	}
	for _, n := range r.Data.Mine.Nodes {
		it := ghPR(n, env)
		it.Kind = "pr_mine_open"
		if n.ReviewDecision == "APPROVED" && n.Mergeable == "MERGEABLE" && ghChecks(n) == "SUCCESS" {
			it.Kind = "pr_mine_ready"
		}
		items = append(items, it)
	}
	for _, n := range r.Data.Issues.Nodes {
		items = append(items, Item{
			ID: fmt.Sprintf("github:issue:%s#%d", n.Repository.NameWithOwner, n.Number), Source: "github",
			Kind: "issue_assigned", Title: oneLine(n.Title), URL: n.URL, Who: []string{n.Author.Login},
			When: n.UpdatedAt.In(env.Loc), Body: plainBody(n.Body),
			Facts: compact([]string{"repo: " + n.Repository.NameWithOwner, age(env.Now, n.UpdatedAt), jiraFact(n.Title, "")}),
		})
	}
	return items, nil
}

func ghPR(n ghNode, env Env) Item {
	who := []string{n.Author.Login}
	for _, rv := range n.Reviews.Nodes {
		if !slices.Contains(who, rv.Author.Login) {
			who = append(who, rv.Author.Login)
		}
	}
	mergeable := map[string]string{"MERGEABLE": "yes", "CONFLICTING": "no (conflicts)"}[n.Mergeable]
	if mergeable == "" {
		mergeable = "unknown"
	}
	review := n.ReviewDecision
	if review == "" {
		review = "none required"
	}
	return Item{
		ID: fmt.Sprintf("github:pr:%s#%d", n.Repository.NameWithOwner, n.Number), Source: "github",
		Title: oneLine(n.Title), URL: n.URL, Who: who, When: n.UpdatedAt.In(env.Loc), Body: plainBody(n.Body),
		Facts: compact([]string{
			"repo: " + n.Repository.NameWithOwner,
			fmt.Sprintf("approvals: %d", n.Reviews.TotalCount),
			"review: " + review,
			"checks: " + strings.ToLower(ghChecks(n)),
			"mergeable: " + mergeable,
			age(env.Now, n.UpdatedAt),
			jiraFact(n.Title, n.HeadRefName),
		}),
	}
}

func ghChecks(n ghNode) string {
	if len(n.Commits.Nodes) == 0 || n.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return "NONE"
	}
	return n.Commits.Nodes[0].Commit.StatusCheckRollup.State
}

// age is a fact computed in Go, so the LLM never does date arithmetic.
func age(now, t time.Time) string {
	d := int(now.Sub(t).Hours() / 24)
	if d <= 0 {
		return "updated: today"
	}
	return fmt.Sprintf("updated: %dd ago", d)
}

var jiraKeyRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-[0-9]+\b`)

// jiraFact links a PR to its ticket for free: a key in the title or branch name.
func jiraFact(title, branch string) string {
	if k := jiraKeyRe.FindString(title); k != "" {
		return "jira: " + k
	}
	if k := jiraKeyRe.FindString(strings.ToUpper(branch)); k != "" {
		return "jira: " + k
	}
	return ""
}

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	spacesRe      = regexp.MustCompile(`[ \t]+`)
	blankLinesRe  = regexp.MustCompile(`\n{3,}`)
)

// plainBody flattens a markdown body enough for a prompt: no PR-template comments, no runs
// of blank lines, capped at maxBody characters.
func plainBody(s string) string {
	s = htmlCommentRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = spacesRe.ReplaceAllString(s, " ")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	return truncate(strings.TrimSpace(s), maxBody)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func oneLine(s string) string {
	return truncate(strings.Join(strings.Fields(s), " "), maxTitle)
}

func compact(ss []string) []string {
	return slices.DeleteFunc(ss, func(s string) bool { return s == "" })
}
