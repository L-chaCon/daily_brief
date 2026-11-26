package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDecodeSlackSearch(t *testing.T) {
	b, err := os.ReadFile("testdata/slack/search_dm.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodeSlackSearch(b)
	if err != nil {
		t.Fatal(err)
	}
	msgs := p.Msgs
	if p.More || p.Cursor != "" {
		t.Errorf("no pagination_info: more %v, cursor %q", p.More, p.Cursor)
	}
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5", len(msgs))
	}
	m := msgs[0]
	want := slackMsg{Channel: "D0FAKE0001", From: "Kyrone Example", TS: "1791909000.000100",
		Permalink: "https://acme.slack.com/archives/D0FAKE0001/p1791909000000100",
		Text:      "Can you look at the rollout plan before standup?\nIt's in <https://docs.example.com/plan|the plan doc>, thanks <@U0FAKEME01>"}
	if m.Channel != want.Channel || m.From != want.From || m.TS != want.TS || m.Permalink != want.Permalink || m.Text != want.Text || m.Bot {
		t.Errorf("msg[0] = %+v\nwant     %+v", m, want)
	}
	if m.People["U0FAKEME01"] != "Pat Example" || m.People["U0FAKEKY01"] != "Kyrone Example" {
		t.Errorf("participants = %v", m.People)
	}
	if !msgs[1].Bot {
		t.Error("msg[1]: [BOT] tag not decoded")
	}
	if msgs[2].Text != "" {
		t.Errorf("msg[2]: empty text = %q", msgs[2].Text)
	}

	b, _ = os.ReadFile("testdata/slack/search_dm_more.json")
	if p, _ := decodeSlackSearch(b); !p.More || p.Cursor != "ZmFrZWN1cnNvcg==" {
		t.Errorf("pagination_info set: more %v, cursor %q", p.More, p.Cursor)
	}

	b, _ = os.ReadFile("testdata/slack/search_empty.json")
	if p, err := decodeSlackSearch(b); err != nil || len(p.Msgs) != 0 {
		t.Errorf("empty search: %d msgs, %v", len(p.Msgs), err)
	}

	// One odd result among good ones: decode keeps it (translates only); build skips and counts it.
	odd := `{"results":"# Search Results for: \n### Result 1 of 2\nChannel: DM (ID: D0FAKE0001)\nMessage_ts: 1791909000.000100\nPermalink: [link](https://acme.slack.com/archives/D0FAKE0001/p1791909000000100)\nText: \n> fine\n\n---\n### Result 2 of 2\nChannel: DM (ID: D0FAKE0002)\nText: \n> no ts\n","pagination_info":""}`
	p, err = decodeSlackSearch([]byte(odd))
	if err != nil || len(p.Msgs) != 2 {
		t.Fatalf("one odd result: %d msgs, %v (one bad result must not cost the page)", len(p.Msgs), err)
	}
	env := testEnv(t)
	if items, unreadable := slackItems(p.Msgs, "slack_dm", env.Now.Add(-24*time.Hour), env); len(items) != 1 || unreadable != 1 {
		t.Errorf("build: %d items, %d unreadable, want 1 and 1", len(items), unreadable)
	}

	// A layout we don't recognise fails loudly; it never looks like "no messages".
	for name, body := range map[string]string{
		"not json":      `Slack is down`,
		"unknown text":  `{"results":"Here are your messages: ...","pagination_info":""}`,
		"block, no ts":  `{"results":"# Search Results for: \n### Result 1 of 1\nChannel: DM (ID: D0FAKE0001)\nPermalink: [link](https://acme.slack.com/archives/D0FAKE0001/p1)\nText: \n> hi\n","pagination_info":""}`,
		"block, no url": `{"results":"# Search Results for: \n### Result 1 of 1\nChannel: DM (ID: D0FAKE0001)\nMessage_ts: 1791909000.000100\nText: \n> hi\n","pagination_info":""}`,
	} {
		if _, err := decodeSlackSearch([]byte(body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestSlackItems(t *testing.T) {
	env := testEnv(t) // now = 2026-10-14 08:00 Europe/London
	b, _ := os.ReadFile("testdata/slack/search_dm.json")
	p, err := decodeSlackSearch(b)
	if err != nil {
		t.Fatal(err)
	}
	items, unreadable := slackItems(p.Msgs, "slack_dm", env.Now.Add(-24*time.Hour), env)
	if unreadable != 0 {
		t.Errorf("unreadable = %d", unreadable)
	}
	ids := []string{}
	byID := map[string]Item{}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			t.Error(err)
		}
		ids = append(ids, it.ID)
		byID[it.ID] = it
	}
	// bot DM skipped; the two-day-old DM is outside the last 24 h.
	wantIDs := "slack:msg:D0FAKE0001/1791909000.000100 slack:msg:D0FAKE0003/1791960300.000100 slack:msg:D0FAKE0004/1791960600.000100"
	if strings.Join(ids, " ") != wantIDs {
		t.Fatalf("ids = %v\nwant %s", ids, wantIDs)
	}

	it := byID["slack:msg:D0FAKE0001/1791909000.000100"]
	if it.Source != "slack" || it.Kind != "slack_dm" || it.URL != "https://acme.slack.com/archives/D0FAKE0001/p1791909000000100" {
		t.Errorf("item = %+v", it)
	}
	if it.Title != "Can you look at the rollout plan before standup?" {
		t.Errorf("title = %q", it.Title)
	}
	if it.Body != "Can you look at the rollout plan before standup?\nIt's in the plan doc, thanks @Pat Example" {
		t.Errorf("body = %q", it.Body)
	}
	if strings.Join(it.Who, ",") != "Kyrone Example" {
		t.Errorf("who = %v", it.Who)
	}
	if it.When.Location() != env.Loc || it.When.Format("2006-01-02 15:04") != "2026-10-13 17:30" {
		t.Errorf("when = %v, want 17:30 BST in env.Loc (from Message_ts, not the GMT label)", it.When)
	}

	// No text: the title still says something.
	if got := byID["slack:msg:D0FAKE0003/1791960300.000100"].Title; got != "Message from Sam Example" {
		t.Errorf("empty-text title = %q", got)
	}
}

func TestSlackMarkup(t *testing.T) {
	people := map[string]string{"U0FAKEME01": "Pat Example"}
	for in, want := range map[string]string{
		"thanks <@U0FAKEME01>":                       "thanks @Pat Example",
		"ping <@U0FAKEXX99>":                         "ping @someone",
		"see <#C0FAKECH1|team-ops>":                  "see #team-ops",
		"in <https://docs.example.com/plan|the doc>": "in the doc",
		"at <https://example.com/x>":                 "at https://example.com/x",
		"hey <!here> and <!channel>":                 "hey @here and @channel",
		"<!subteam^S0FAKE|@ops-team> please":         "@ops-team please",
		"*bold* and _it_ stay as typed":              "*bold* and _it_ stay as typed",
		"a &lt;b&gt; &amp; c":                        "a <b> & c",
	} {
		if got := slackPlain(in, people); got != want {
			t.Errorf("slackPlain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollectSlack(t *testing.T) {
	env := testEnv(t)
	env.RunDir = t.TempDir()
	full, _ := os.ReadFile("testdata/slack/search_dm_more.json")
	// recent = the same page without the two-day-old DM: every result inside the window.
	var r struct {
		Results        string `json:"results"`
		PaginationInfo string `json:"pagination_info"`
	}
	_ = json.Unmarshal(full, &r)
	r.Results = strings.Join(slices.DeleteFunc(strings.Split(r.Results, "### "), func(b string) bool { return strings.Contains(b, "old message") }), "### ")
	recent, _ := json.Marshal(r)
	last, _ := os.ReadFile("testdata/slack/search_empty.json")

	pages := map[string][]byte{} // cursor ("" = first page) → tool output
	var calls []string
	env.Run = fakeClaude(t, func(_ string, in map[string]any) []byte {
		// The query is fixed by Go: DMs to me since two days before today (Slack's after: is exclusive).
		if in["filters"] != "to:me after:2026-10-12" {
			t.Errorf("filters = %v", in["filters"])
		}
		cursor, _ := in["cursor"].(string)
		calls = append(calls, cursor)
		return pages[cursor]
	})

	// More pages exist, but this page already reaches past the 24 h window: one call, no note.
	pages[""] = full
	items, err := collectSlack(context.Background(), env)
	if len(items) != 3 || err != nil || len(calls) != 1 {
		t.Errorf("window covered: %d items, %d calls, err %v", len(items), len(calls), err)
	}

	// Every result is inside the window: follow the cursor until the window is covered.
	calls = nil
	pages[""], pages["ZmFrZWN1cnNvcg=="] = recent, last
	items, err = collectSlack(context.Background(), env)
	if len(items) != 3 || err != nil || strings.Join(calls, ",") != ",ZmFrZWN1cnNvcg==" {
		t.Errorf("paging: %d items, calls %q, err %v", len(items), calls, err)
	}

	// Still more after slackMaxPages: keep what we have, and say so. (The same page again → duplicate
	// ids, which collectAll drops and counts; here we only check the paging stops.)
	calls = nil
	pages["ZmFrZWN1cnNvcg=="] = recent
	_, err = collectSlack(context.Background(), env)
	if len(calls) != slackMaxPages || err == nil || !strings.Contains(err.Error(), "more than 60") {
		t.Errorf("cap: calls %q, err %v", calls, err)
	}
}
