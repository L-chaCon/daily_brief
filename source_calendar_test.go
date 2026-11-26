package main

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDecodeGoogleEvents(t *testing.T) {
	env := testEnv(t)
	b, err := os.ReadFile("testdata/calendar/google_list_events.json")
	if err != nil {
		t.Fatal(err)
	}
	evs, more, err := decodeGoogleEvents(b, env.Loc)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(evs) != 10 {
		t.Fatalf("got %d events (more %v), want 10: decode translates, it never filters", len(evs), more)
	}
	byUID := map[string]calEvent{}
	for _, e := range evs {
		byUID[e.UID] = e
	}

	all := byUID["fake0allday1"]
	if !all.AllDay || !all.Start.Equal(time.Date(2026, 10, 14, 0, 0, 0, 0, env.Loc)) || !all.End.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, env.Loc)) {
		t.Errorf("all-day = %+v, want local midnight to midnight", all)
	}
	su := byUID["fake0standup_20261014T080000Z"]
	if !su.Recurring || su.Video != "https://meet.google.com/abc-defg-hij" || len(su.Attendees) != 4 || !su.Attendees[3].Resource || su.URL == "" {
		t.Errorf("standup = %+v", su)
	}
	if a := su.Attendees[0]; !a.Self || a.Status != "accepted" {
		t.Errorf("self attendee = %+v", a)
	}
	if a := su.Attendees[2]; a.Name != "sam.example" || strings.Contains(a.Name, "@") {
		t.Errorf("no display name: want the email's local part, never the address: %+v", a)
	}
	rv := byUID["fake0review"]
	if !rv.Start.Equal(time.Date(2026, 10, 14, 13, 0, 0, 0, time.UTC)) || rv.Location != "Room 4 / https://zoom.us/j/123" || !strings.Contains(rv.Notes, "<br>") {
		t.Errorf("review = %+v (Z times parse; notes are passed through raw)", rv)
	}
	if byUID["fake0cancelled"].Status != "cancelled" || byUID["fake0ooo"].Type != "out_of_office" || byUID["fake0notitle"].Title != "" {
		t.Error("status, type or missing title not translated")
	}

	b, _ = os.ReadFile("testdata/calendar/google_list_events_more.json")
	if _, more, _ := decodeGoogleEvents(b, env.Loc); !more {
		t.Error("nextPageToken: want more")
	}
	for name, body := range map[string]string{
		"not json":  `Calendar is down`,
		"no events": `{"accessRole":"owner"}`,
	} {
		if _, _, err := decodeGoogleEvents([]byte(body), env.Loc); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if evs, _, err := decodeGoogleEvents([]byte(`{"events":[]}`), env.Loc); err != nil || len(evs) != 0 {
		t.Errorf("empty day: %v, %v", evs, err)
	}
}

func TestCalendarItems(t *testing.T) {
	env := testEnv(t) // run date 2026-10-14, Europe/London (BST)
	b, _ := os.ReadFile("testdata/calendar/google_list_events.json")
	evs, _, err := decodeGoogleEvents(b, env.Loc)
	if err != nil {
		t.Fatal(err)
	}
	items, unreadable := calendarItems(evs, civilDate{2026, 10, 14}, env)
	if unreadable != 0 {
		t.Errorf("unreadable = %d", unreadable)
	}
	byID := map[string]Item{}
	var ids []string
	for _, it := range items {
		if err := it.Validate(); err != nil {
			t.Error(err)
		}
		byID[it.ID] = it
		ids = append(ids, it.ID)
	}
	// Skipped: declined by me, cancelled, working location.
	want := []string{
		"calendar:event:fake0allday1@2026-10-14T00:00:00+01:00",
		"calendar:event:fake0multiday@2026-10-12T00:00:00+01:00",
		"calendar:event:fake0started@2026-10-13T22:00:00+01:00",
		"calendar:event:fake0standup_20261014T080000Z@2026-10-14T09:00:00+01:00",
		"calendar:event:fake0review@2026-10-14T14:00:00+01:00",
		"calendar:event:fake0ooo@2026-10-15T13:00:00+01:00",
		"calendar:event:fake0notitle@2026-10-15T10:00:00+01:00",
	}
	if strings.Join(ids, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ids:\n%s\nwant:\n%s", strings.Join(ids, "\n"), strings.Join(want, "\n"))
	}

	tests := []struct {
		id, kind, title string
		who             string
		facts           []string
	}{
		{want[0], "event_all_day", "Team offsite prep", "", nil},
		{want[1], "event_all_day", "Conference week", "", []string{"until: Fri 16 Oct"}},
		{want[2], "event", "Overnight batch watch", "", []string{"started: Tue 13 Oct 22:00"}},
		{want[3], "event", "Team Daily Standup", "Kyrone Example,sam.example", []string{"attendees: 3", "recurring: yes", "video: meet"}}, // the room is not a person
		{want[4], "event", "Design review: <b>billing</b>", "Kyrone Example", []string{"attendees: 2", "video: zoom", "my response: not yet"}},
		{want[5], "event", "Out of office", "", []string{"type: out of office"}},
		{want[6], "event", "Busy", "", nil},
	}
	for _, tt := range tests {
		it := byID[tt.id]
		if it.Kind != tt.kind || it.Title != tt.title || strings.Join(it.Who, ",") != tt.who {
			t.Errorf("%s: kind %q title %q who %v", tt.id, it.Kind, it.Title, it.Who)
		}
		if strings.Join(it.Facts, "|") != strings.Join(tt.facts, "|") {
			t.Errorf("%s: facts %q, want %q", tt.id, it.Facts, tt.facts)
		}
	}

	// A multi-day event that started earlier is shown from the window start; its id keeps the real start.
	if md := byID[want[1]]; !md.When.Equal(time.Date(2026, 10, 14, 0, 0, 0, 0, env.Loc)) {
		t.Errorf("multi-day when = %v, want clamped to the window start", md.When)
	}
	if st := byID[want[2]]; !st.When.Equal(time.Date(2026, 10, 14, 0, 0, 0, 0, env.Loc)) {
		t.Errorf("still-running timed event: when = %v, want clamped to the window start", st.When)
	}
	rv := byID[want[4]]
	if rv.When.Location() != env.Loc || rv.End == nil || rv.End.In(env.Loc).Format("15:04") != "15:00" {
		t.Errorf("review when/end = %v/%v, want 14:00–15:00 in env.Loc", rv.When, rv.End)
	}
	if rv.Body != "Agenda:\n1. Limits & quotas\nDoc: here" {
		t.Errorf("body = %q (HTML flattened)", rv.Body)
	}
	if rv.URL != "https://www.google.com/calendar/event?eid=ZmFrZSfake0r" {
		t.Errorf("url = %q, want the event's own htmlLink", rv.URL)
	}
}

// The window is two civil days in env.Loc, so a DST change inside it moves no event in or out wrongly.
func TestCalendarWindowDST(t *testing.T) {
	env := testEnv(t)
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
	ev := func(uid, start string) calEvent {
		return calEvent{UID: uid, Title: uid, Start: at(start), End: at(start).Add(30 * time.Minute)}
	}
	// 2026-10-25: clocks go back at 02:00 BST → 01:00 GMT. Window = [25th 00:00 BST, 27th 00:00 GMT).
	evs := []calEvent{
		ev("before", "2026-10-24T22:30:00Z"),     // 23:30 BST on the 24th
		ev("firstmin", "2026-10-24T23:00:00Z"),   // 00:00 BST on the 25th
		ev("aftershift", "2026-10-26T09:00:00Z"), // 09:00 GMT on the 26th
		ev("lastmin", "2026-10-26T23:59:00Z"),    // 23:59 GMT on the 26th
		ev("after", "2026-10-27T00:00:00Z"),      // 00:00 GMT on the 27th
	}
	items, _ := calendarItems(evs, civilDate{2026, 10, 25}, env)
	var got []string
	for _, it := range items {
		got = append(got, it.Title)
	}
	if strings.Join(got, ",") != "firstmin,aftershift,lastmin" {
		t.Errorf("in window: %v", got)
	}

	// All-day events on 25 h and 23 h days: one day is one day ("until" only for real multi-day events).
	allDay := func(uid string, y int, m time.Month, d, days int) calEvent {
		s := time.Date(y, m, d, 0, 0, 0, 0, env.Loc)
		return calEvent{UID: uid, Title: uid, AllDay: true, Start: s, End: s.AddDate(0, 0, days)}
	}
	for _, tt := range []struct {
		ev    calEvent
		day   civilDate
		facts string
	}{
		{allDay("autumn1", 2026, 10, 25, 1), civilDate{2026, 10, 25}, ""}, // 25 h day
		{allDay("spring1", 2026, 3, 29, 1), civilDate{2026, 3, 29}, ""},   // 23 h day
		{allDay("spring2", 2026, 3, 29, 2), civilDate{2026, 3, 29}, "until: Mon 30 Mar"},
	} {
		items, _ := calendarItems([]calEvent{tt.ev}, tt.day, env)
		if len(items) != 1 || strings.Join(items[0].Facts, "|") != tt.facts {
			t.Errorf("%s: %+v, want facts %q", tt.ev.UID, items, tt.facts)
		}
	}
}

func TestCollectCalendar(t *testing.T) {
	env := testEnv(t)
	env.RunDir = t.TempDir()
	page, _ := os.ReadFile("testdata/calendar/google_list_events_more.json")
	var gotTool string
	var gotArgs map[string]any
	env.Run = fakeClaude(t, func(tool string, in map[string]any) []byte {
		gotTool, gotArgs = tool, in
		return page
	})
	items, err := collectCalendar(context.Background(), env)
	if len(items) != 7 {
		t.Errorf("got %d items, want 7", len(items))
	}
	if err == nil || !strings.Contains(err.Error(), "more than 100") {
		t.Errorf("nextPageToken: want a note, got %v", err)
	}
	// Run date 00:00 → +2 days, local time without offset plus the zone, as the connector wants.
	want, _ := normJSON(map[string]any{"startTime": "2026-10-14T00:00:00", "endTime": "2026-10-16T00:00:00",
		"timeZone": "Europe/London", "orderBy": "startTime", "pageSize": 100})
	if gotTool != "mcp__claude_ai_Google_Calendar__list_events" || !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("call = %s %v", gotTool, gotArgs)
	}

	// --date picks the day, not the clock.
	env.Date = civilDate{2026, 10, 20}
	page = []byte(`{"events":[]}`)
	if _, err := collectCalendar(context.Background(), env); err != nil {
		t.Error(err)
	}
	if gotArgs["startTime"] != "2026-10-20T00:00:00" || gotArgs["endTime"] != "2026-10-22T00:00:00" {
		t.Errorf("--date ignored: %v", gotArgs)
	}
}
