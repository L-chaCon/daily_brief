package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Slack via the claude.ai Slack connector (D23, D24; doc 03 §6). Three layers:
//   fetch  (callConnector: claude makes one search, Go gets the raw result)
//   decode (decodeSlackSearch: the connector's text layout → []slackMsg, translates only)
//   build  (slackItems: every decision: window, bots, ids, markup, links)
// Basic scope: DMs to you. Mentions and thread replies come later, as more rows here.

const (
	slackLimit    = 20 // the connector's maximum page size
	slackMaxPages = 3  // ~6 s and ~$0.005 per page
)

var slackQueries = []struct{ kind, filters string }{
	{"slack_dm", "to:me after:%s"},
}

func collectSlack(ctx context.Context, env Env) ([]Item, error) {
	// Slack's after: is exclusive and by day: ask from two days back, then slackItems keeps the last 24 h.
	after := env.Now.In(env.Loc).AddDate(0, 0, -2).Format(time.DateOnly)
	since := env.Now.Add(-24 * time.Hour)
	var items []Item
	var errs []error
	for _, q := range slackQueries {
		args := map[string]any{"filters": fmt.Sprintf(q.filters, after), "include_context": false, "limit": slackLimit}
		for page := 1; ; page++ {
			out, err := callConnector(ctx, env, "slack", "mcp__claude_ai_Slack__slack_search_public_and_private", args)
			if err != nil {
				errs = append(errs, err)
				break
			}
			p, err := decodeSlackSearch(out)
			if err != nil {
				errs = append(errs, err)
				break
			}
			got, unreadable := slackItems(p.Msgs, q.kind, since, env)
			items = append(items, got...)
			if unreadable > 0 {
				errs = append(errs, fmt.Errorf("slack: %d results without channel, Message_ts or permalink skipped", unreadable))
			}
			if !p.More || reachesBefore(p.Msgs, since) { // done, or this page already covers the window
				break
			}
			if p.Cursor == "" || page == slackMaxPages {
				errs = append(errs, fmt.Errorf("slack: more than %d %s results in the last 24 h, showing the newest %d", page*slackLimit, q.kind, page*slackLimit))
				break
			}
			args["cursor"] = p.Cursor
		}
	}
	return items, errors.Join(errs...)
}

// reachesBefore reports whether any message (bots included) is older than t: if so, the page
// already covers the whole window and further pages don't matter.
func reachesBefore(msgs []slackMsg, t time.Time) bool {
	for _, m := range msgs {
		if when, err := slackTime(m.TS); err == nil && when.Before(t) {
			return true
		}
	}
	return false
}

// slackPage is one page of search results. Cursor is "" when the connector didn't say how to continue.
type slackPage struct {
	Msgs   []slackMsg
	More   bool
	Cursor string
}

// slackMsg is one search result, as the connector reports it.
type slackMsg struct {
	Channel   string            // channel id: D… for DMs
	From      string            // display name
	Bot       bool              // the connector tags bot senders [BOT]
	TS        string            // Message_ts, Slack's message id within the channel
	Permalink string            //
	Text      string            // Slack markup, unflattened
	People    map[string]string // user id → name, from Participants and From
}

var (
	slackIDRe     = regexp.MustCompile(`^(.*?)\s*\(ID: ([A-Z0-9]+)\)`)
	slackLinkRe   = regexp.MustCompile(`^\[[^\]]*\]\((https://[^)\s]+)\)$`)
	slackTSRe     = regexp.MustCompile(`^\d+\.\d+$`)
	slackCursorRe = regexp.MustCompile("cursor `([^`]+)`")
)

// decodeSlackSearch reads search_public_and_private's output: JSON {results, pagination_info}, where
// results is markdown with one "### Result i of n" block of "Key: value" lines per message, and the
// message text as "> " lines after "Text:". An unknown layout is an error, never "no messages".
func decodeSlackSearch(b []byte) (slackPage, error) {
	var msgs []slackMsg
	var r struct {
		Results        *string `json:"results"`
		PaginationInfo string  `json:"pagination_info"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Results == nil {
		return slackPage{}, fmt.Errorf("slack: unexpected search output (not {results, pagination_info}): %.80q", b)
	}
	if !strings.HasPrefix(strings.TrimSpace(*r.Results), "# Search Results") {
		return slackPage{}, fmt.Errorf("slack: unexpected search layout: %.80q", *r.Results)
	}
	var cur *slackMsg
	inText := false
	finish := func() {
		if cur != nil {
			cur.Text = strings.TrimSpace(cur.Text)
			msgs = append(msgs, *cur)
		}
	}
	for line := range strings.Lines(*r.Results) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "### ") {
			finish()
			cur, inText = &slackMsg{People: map[string]string{}}, false
			continue
		}
		if cur == nil {
			continue
		}
		if inText {
			switch {
			case line == "---":
				inText = false
			case line == ">" || strings.HasPrefix(line, "> "):
				cur.Text += strings.TrimPrefix(strings.TrimPrefix(line, ">"), " ") + "\n"
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Channel":
			if m := slackIDRe.FindStringSubmatch(val); m != nil {
				cur.Channel = m[2]
			}
		case "Participants":
			for p := range strings.SplitSeq(val, ", ") {
				if m := slackIDRe.FindStringSubmatch(p); m != nil {
					cur.People[m[2]] = m[1]
				}
			}
		case "From":
			cur.Bot = strings.Contains(val, "[BOT]")
			if m := slackIDRe.FindStringSubmatch(val); m != nil {
				cur.From = m[1]
				cur.People[m[2]] = m[1]
			}
		case "Message_ts":
			cur.TS = val
		case "Permalink":
			if m := slackLinkRe.FindStringSubmatch(val); m != nil {
				cur.Permalink = m[1]
			}
		case "Text":
			inText = true
		}
	}
	finish()
	// Decode translates; one odd result is build's problem. But if no result has the fields we read,
	// the layout changed: that's an error, never "no messages".
	if len(msgs) > 0 && !slices.ContainsFunc(msgs, func(m slackMsg) bool { return m.Channel != "" && m.TS != "" && m.Permalink != "" }) {
		return slackPage{}, errors.New("slack: unexpected search layout: no result has Channel ID, Message_ts and Permalink")
	}
	p := slackPage{Msgs: msgs, More: strings.TrimSpace(r.PaginationInfo) != ""}
	if m := slackCursorRe.FindStringSubmatch(r.PaginationInfo); m != nil {
		p.Cursor = m[1]
	}
	return p, nil
}

// slackItems makes every decision: messages since `since` only, bots skipped (they repeat GitHub/Jira
// and alerts), times from Message_ts (the "Time:" line has an ambiguous zone label), links only from
// Slack itself. Kind comes from the query (to:me = DMs, group DMs included). Results missing the fields
// an id needs are skipped and counted.
func slackItems(msgs []slackMsg, kind string, since time.Time, env Env) (items []Item, unreadable int) {
	for _, m := range msgs {
		when, err := slackTime(m.TS)
		if m.Channel == "" || !slackTSRe.MatchString(m.TS) || err != nil {
			unreadable++
			continue
		}
		if m.Bot || when.Before(since) {
			continue
		}
		body := slackPlain(m.Text, m.People)
		title, _, _ := strings.Cut(body, "\n")
		if title = oneLine(title); title == "" {
			title = oneLine("Message from " + cmp.Or(m.From, "someone"))
		}
		items = append(items, Item{
			ID:     "slack:msg:" + m.Channel + "/" + m.TS,
			Source: "slack",
			Kind:   kind,
			Title:  title,
			URL:    slackPermalink(m.Permalink),
			Who:    compact([]string{m.From}),
			When:   when.In(env.Loc),
			Body:   truncate(body, maxBody),
		})
	}
	return items, unreadable
}

func slackTime(ts string) (time.Time, error) {
	sec, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	us, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return time.Unix(s, us*1000), nil
}

// slackPermalink keeps the link only if it's a Slack archive link (D4: links come from the source, checked).
func slackPermalink(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Host, ".slack.com") || !strings.HasPrefix(u.Path, "/archives/") {
		return ""
	}
	return s
}

var slackMarkupRe = regexp.MustCompile(`<([^<>]+)>`)

// slackPlain flattens Slack's message markup into plain text: <@U1> → @Name, <#C1|ops> → #ops,
// <url|label> → label, <url> → url, <!here> → @here. Bold/italics stay as typed.
func slackPlain(s string, people map[string]string) string {
	s = slackMarkupRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		target, label, hasLabel := strings.Cut(inner, "|")
		switch {
		case strings.HasPrefix(target, "@"):
			if name, ok := people[target[1:]]; ok {
				return "@" + name
			}
			return "@someone"
		case strings.HasPrefix(target, "#"):
			return "#" + cmp.Or(label, target[1:])
		case strings.HasPrefix(target, "!"):
			if hasLabel {
				return label
			}
			return "@" + strings.TrimPrefix(target, "!")
		case hasLabel:
			return label
		}
		return target
	})
	return strings.TrimSpace(html.UnescapeString(s))
}
