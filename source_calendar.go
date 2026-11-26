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
	"strings"
	"time"
)

// Calendar (doc 03 §2, plan docs/plans/2026-11-26-connector-boundary.md). The boundary is calEvent:
//
//	transport (fetch)                decode (pure, translates only)        build (pure, decides)
//	claude → Google Calendar   ─▶    decodeGoogleEvents        ─┐
//	icalBuddy (later)          ─▶    decodeICalBuddy (later)   ─┴─▶ []calEvent ─▶ calendarItems ─▶ []Item
//
// Swapping the transport is one line in the sources table. Every decision (window, declined,
// cancelled, self, ids, facts, links) lives in calendarItems, once.

// calEvent is one occurrence of an event, as any transport sees it, in our words. A decoder that
// can't fill a field leaves it zero.
type calEvent struct {
	UID       string    // the transport's stable id for this occurrence (Google: instance id; Calendar.app: UID)
	Title     string    // "" if the transport hides it (private event)
	Start     time.Time // all-day: local midnight
	End       time.Time // all-day: local midnight after the last day (exclusive)
	AllDay    bool
	Recurring bool
	Status    string // "confirmed" | "tentative" | "cancelled" | "" (unknown)
	Type      string // "" (a normal event) | "out_of_office" | "focus_time" | "working_location" | "birthday" | "from_gmail"
	URL       string // the event's own page; "" when the transport has none
	Location  string
	Notes     string // raw description, as the transport has it (may be HTML)
	Video     string // conference link, if the transport has one apart from location/notes
	Attendees []calAttendee
}

type calAttendee struct {
	Name     string // display name, else the email's local part; never an address
	Self     bool
	Resource bool   // a room or equipment, not a person (Google resource:true; iCal CUTYPE=ROOM/RESOURCE)
	Status   string // "accepted" | "declined" | "tentative" | "needsAction" | "" (unknown)
}

// --- transport: claude → claude.ai Google Calendar connector ---------------------------------

const calendarPageSize = 100 // two days; the connector's default page. Big pages overflow claude's context

func collectCalendar(ctx context.Context, env Env) ([]Item, error) {
	day := env.day()
	from := day.In(env.Loc)
	to := from.AddDate(0, 0, 2)
	const local = "2006-01-02T15:04:05" // the connector wants local time + timeZone, no offset
	args := map[string]any{"startTime": from.Format(local), "endTime": to.Format(local),
		"timeZone": env.Loc.String(), "orderBy": "startTime", "pageSize": calendarPageSize}
	out, err := callConnector(ctx, env, "calendar", "mcp__claude_ai_Google_Calendar__list_events", args)
	if err != nil {
		return nil, err
	}
	evs, more, err := decodeGoogleEvents(out, env.Loc)
	if err != nil {
		return nil, err
	}
	items, unreadable := calendarItems(evs, day, env)
	var errs []error
	if unreadable > 0 {
		errs = append(errs, fmt.Errorf("calendar: %d events without id or times skipped", unreadable))
	}
	if more {
		errs = append(errs, fmt.Errorf("calendar: more than %d events in two days, showing the first %d", calendarPageSize, calendarPageSize))
	}
	return items, errors.Join(errs...)
}

// --- decode: Google Calendar API JSON → []calEvent -------------------------------------------

type gTime struct {
	DateTime string `json:"dateTime"` // RFC 3339, with an offset or Z
	Date     string `json:"date"`     // all-day: YYYY-MM-DD
}

type gEvent struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	Summary          string `json:"summary"`
	HTMLLink         string `json:"htmlLink"`
	Start, End       gTime
	RecurringEventID string `json:"recurringEventId"`
	EventType        string `json:"eventType"`
	Location         string `json:"location"`
	Description      string `json:"description"`
	ConferenceURL    string `json:"conferenceUrl"`
	ConferenceData   struct {
		VideoEntryPoint struct {
			URI string `json:"uri"`
		} `json:"videoEntryPoint"`
	} `json:"conferenceData"`
	Attendees []struct {
		Email          string `json:"email"`
		DisplayName    string `json:"displayName"`
		Self           bool   `json:"self"`
		Resource       bool   `json:"resource"`
		ResponseStatus string `json:"responseStatus"`
	} `json:"attendees"`
}

var gEventTypes = map[string]string{
	"": "", "DEFAULT": "", "EVENT_TYPE_UNSPECIFIED": "", "OUT_OF_OFFICE": "out_of_office", "FOCUS_TIME": "focus_time",
	"WORKING_LOCATION": "working_location", "BIRTHDAY": "birthday", "FROM_GMAIL": "from_gmail",
}

// decodeGoogleEvents reads list_events' output. loc is only for all-day dates, which have no zone.
func decodeGoogleEvents(b []byte, loc *time.Location) (evs []calEvent, more bool, err error) {
	var r struct {
		Events        *[]gEvent `json:"events"`
		NextPageToken string    `json:"nextPageToken"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Events == nil {
		return nil, false, fmt.Errorf("calendar: unexpected list_events output (no events list): %.80q", b)
	}
	for _, g := range *r.Events {
		e := calEvent{
			UID: g.ID, Title: g.Summary, Status: g.Status, URL: g.HTMLLink, Location: g.Location, Notes: g.Description,
			Recurring: g.RecurringEventID != "",
			Video:     cmp.Or(g.ConferenceURL, g.ConferenceData.VideoEntryPoint.URI),
		}
		if t, ok := gEventTypes[g.EventType]; ok {
			e.Type = t
		} else {
			e.Type = strings.ToLower(g.EventType)
		}
		e.Start, e.AllDay = gParse(g.Start, loc)
		e.End, _ = gParse(g.End, loc)
		for _, a := range g.Attendees {
			name := a.DisplayName
			if name == "" {
				name, _, _ = strings.Cut(a.Email, "@")
			}
			e.Attendees = append(e.Attendees, calAttendee{Name: name, Self: a.Self, Resource: a.Resource, Status: a.ResponseStatus})
		}
		evs = append(evs, e)
	}
	return evs, r.NextPageToken != "", nil
}

// gParse returns the zero time when the value is missing or malformed; build counts that as unreadable.
func gParse(t gTime, loc *time.Location) (time.Time, bool) {
	if t.Date != "" {
		d, err := time.ParseInLocation(time.DateOnly, t.Date, loc)
		if err != nil {
			return time.Time{}, true
		}
		return d, true
	}
	d, _ := time.Parse(time.RFC3339, t.DateTime)
	return d, false
}

// --- build: []calEvent → []Item. Every decision is here, for every transport. ----------------

// calendarItems keeps events of the run date and the next day (doc 02 §4) in env.Loc. Skipped:
// cancelled, declined by me, working-location markers (daily noise). Events still running from
// before the window are shown from the window start; the id keeps the real start, and a fact says when it began.
func calendarItems(evs []calEvent, day civilDate, env Env) (items []Item, unreadable int) {
	from := day.In(env.Loc)
	to := from.AddDate(0, 0, 2)
	for _, e := range evs {
		if e.UID == "" || e.Start.IsZero() || e.End.IsZero() || e.End.Before(e.Start) {
			unreadable++
			continue
		}
		if e.Status == "cancelled" || e.Type == "working_location" || myStatus(e) == "declined" || !e.End.After(from) || !e.Start.Before(to) {
			continue
		}
		when := e.Start
		if when.Before(from) {
			when = from
		}
		end := e.End.In(env.Loc)
		it := Item{
			ID:     "calendar:event:" + e.UID + "@" + e.Start.In(env.Loc).Format(time.RFC3339),
			Source: "calendar",
			Kind:   "event",
			Title:  oneLine(cmp.Or(e.Title, "Busy")),
			URL:    calendarLink(e.URL),
			When:   when.In(env.Loc),
			End:    &end,
			Body:   plainBody(htmlText(e.Notes)),
		}
		if e.AllDay {
			it.Kind = "event_all_day"
		}
		var n int
		for _, a := range e.Attendees {
			if a.Resource {
				continue
			}
			n++
			if !a.Self && a.Name != "" && len(it.Who) < 10 {
				it.Who = append(it.Who, a.Name)
			}
		}
		lastDay := e.End.In(env.Loc).AddDate(0, 0, -1) // all-day end is exclusive; civil dates, so DST days don't matter
		switch {
		case e.AllDay && dateOf(lastDay) != dateOf(e.Start.In(env.Loc)):
			it.Facts = append(it.Facts, "until: "+lastDay.Format("Mon 2 Jan"))
		case !e.AllDay && e.Start.Before(from):
			it.Facts = append(it.Facts, "started: "+e.Start.In(env.Loc).Format("Mon 2 Jan 15:04"))
		}
		if n > 0 {
			it.Facts = append(it.Facts, fmt.Sprintf("attendees: %d", n))
		}
		if e.Recurring {
			it.Facts = append(it.Facts, "recurring: yes")
		}
		if v := videoKind(e.Video, e.Location, e.Notes); v != "" {
			it.Facts = append(it.Facts, "video: "+v)
		}
		if e.Type != "" {
			it.Facts = append(it.Facts, "type: "+strings.ReplaceAll(e.Type, "_", " "))
		}
		if loc := oneLine(e.Location); loc != "" && !strings.Contains(loc, "://") && !emailRe.MatchString(loc) {
			it.Facts = append(it.Facts, "location: "+truncate(loc, 80))
		}
		switch myStatus(e) {
		case "needsAction":
			it.Facts = append(it.Facts, "my response: not yet")
		case "tentative":
			it.Facts = append(it.Facts, "my response: maybe")
		}
		items = append(items, it)
	}
	return items, unreadable
}

func myStatus(e calEvent) string {
	for _, a := range e.Attendees {
		if a.Self {
			return a.Status
		}
	}
	return ""
}

// videoKind names the call provider from the conference link, or a link in location/notes. Never the link.
func videoKind(video, location, notes string) string {
	for _, s := range []string{video, location, notes} {
		switch {
		case strings.Contains(s, "meet.google.com"):
			return "meet"
		case strings.Contains(s, "zoom.us"):
			return "zoom"
		case strings.Contains(s, "teams.microsoft.com"):
			return "teams"
		}
	}
	return ""
}

// calendarLink keeps the transport's own link to the event if it's https (D4: links come from the
// source). No host check: each transport has its own (Google, iCal URL property).
func calendarLink(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return s
}

var (
	htmlBreakRe = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>`)
	anyTagRe    = regexp.MustCompile(`<[^>]*>`)
)

// htmlText flattens the little HTML calendar descriptions use: breaks become newlines, tags go,
// entities are decoded. Not an HTML parser: good enough for a prompt excerpt.
func htmlText(s string) string {
	s = htmlBreakRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}
