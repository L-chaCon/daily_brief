package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"slices"
	"strings"
	"time"
)

// Page is the join of Raw + Brief that the template prints (doc 02 §7).
// Everything is preformatted here, so the template never computes anything.
type Page struct {
	Date        string // "2026-10-14"
	Weekday     string // "Wednesday"
	Greeting    string
	Focus       *Card
	Todos       []Card
	Updates     []Card
	Schedule    []DayPart
	Tomorrow    []Event
	Reading     []Card
	Sources     []SourceStatus
	Degraded    bool
	DegradedWhy string
	GeneratedAt string // "08:00"
	ThinkInfo   string // "sonnet · ~/.claude-work": which model and Claude account wrote the brief
}

type Card struct {
	ID     string // checkbox key
	Label  string
	Title  string
	Body   string
	Prompt string
	Links  []Link
	Who    []string
}

type Link struct{ Text, URL, Source string }

type DayPart struct {
	Name   string
	Events []Event
}

type Event struct {
	ID, Title, Start, End string
	AllDay                bool
	Note, PrepPrompt      string
}

var sourceNames = map[string]string{
	"github": "GitHub", "jira": "Jira", "calendar": "Calendar", "mail": "Mail", "slack": "Slack",
	"youtube": "YouTube", "feed": "Feed", "arxiv": "arXiv", "hn": "Hacker News", "hfpapers": "HF Papers", "notes": "Notes",
}

func sourceName(s string) string {
	if n, ok := sourceNames[s]; ok {
		return n
	}
	return s
}

// join is the ONLY place prose meets facts (decision D4). b == nil means think failed or was
// skipped: the page is then built deterministically from raw alone. Ids that aren't in raw are
// skipped (Validate should have caught them already).
func join(raw Raw, b *Brief, thinkErr error) Page {
	loc, err := time.LoadLocation(raw.Timezone)
	if err != nil {
		loc = time.Local
	}
	day := raw.Date.In(loc)
	items := make(map[string]Item, len(raw.Items))
	for _, it := range raw.Items {
		items[it.ID] = it
	}
	card := func(ids []string) Card {
		c := Card{}
		for _, id := range ids {
			it, ok := items[id]
			if !ok {
				continue
			}
			if c.ID == "" {
				c.ID = id
			}
			if it.URL != "" {
				c.Links = append(c.Links, Link{Text: sourceName(it.Source), URL: it.URL, Source: it.Source})
			}
			for _, w := range it.Who {
				if !slices.Contains(c.Who, w) {
					c.Who = append(c.Who, w)
				}
			}
		}
		return c
	}

	p := Page{
		Date:        raw.Date.String(),
		Weekday:     day.Weekday().String(),
		Sources:     raw.Sources,
		GeneratedAt: raw.GeneratedAt.In(loc).Format("15:04"),
	}

	notes := map[string]ScheduleNote{}
	if b != nil {
		for _, n := range b.ScheduleNotes {
			notes[n.ItemID] = n
		}
	}
	p.Schedule, p.Tomorrow = schedule(raw.Items, day, loc, notes)

	if b == nil {
		p.Degraded = true
		p.DegradedWhy = "no brief.json"
		if thinkErr != nil {
			p.DegradedWhy, _, _ = strings.Cut(thinkErr.Error(), "\n")
		}
		p.Greeting = fallbackGreeting(raw.Items, day, loc)
		p.Todos, p.Reading = degradedCards(raw.Items, card)
		return p
	}

	p.Greeting = b.Greeting
	if f := b.Focus; f != nil {
		c := card(f.ItemIDs)
		c.Title, c.Body, c.Prompt = f.Title, f.Body, f.Prompt
		c.ID = "focus:" + c.ID // its own checkbox, separate from the todo for the same item
		p.Focus = &c
	}
	for _, t := range b.Todos {
		c := card([]string{t.ItemID})
		c.Label, c.Title, c.Body = t.Label, t.Title, t.Body
		p.Todos = append(p.Todos, c)
	}
	for _, u := range b.Updates {
		c := card(u.ItemIDs)
		c.Title, c.Body = u.Title, u.Body
		p.Updates = append(p.Updates, c)
	}
	for _, r := range b.Reading {
		c := card([]string{r.ItemID})
		c.Title, c.Body = items[r.ItemID].Title, r.Why
		p.Reading = append(p.Reading, c)
	}
	return p
}

// schedule is the deterministic skeleton: every calendar event today, whether or not the LLM
// mentioned it, split into day parts; plus the first 3 events tomorrow. The LLM only adds notes.
func schedule(items []Item, day time.Time, loc *time.Location, notes map[string]ScheduleNote) ([]DayPart, []Event) {
	parts := []DayPart{{Name: "Morning"}, {Name: "Afternoon"}, {Name: "Evening"}}
	tomorrowStart, dayAfter := day.AddDate(0, 0, 1), day.AddDate(0, 0, 2)
	var tomorrow []Event
	var cal []Item
	for _, it := range items {
		if it.Source == "calendar" {
			cal = append(cal, it)
		}
	}
	slices.SortStableFunc(cal, func(a, b Item) int { return a.When.Compare(b.When) })
	for _, it := range cal {
		when := it.When.In(loc)
		ev := Event{ID: it.ID, Title: it.Title, AllDay: it.Kind == "event_all_day"}
		if !ev.AllDay {
			ev.Start = when.Format("15:04")
			if it.End != nil {
				ev.End = it.End.In(loc).Format("15:04")
			}
		}
		n := notes[it.ID]
		ev.Note, ev.PrepPrompt = n.Note, n.PrepPrompt
		switch {
		case !when.Before(day) && when.Before(tomorrowStart):
			i := 0 // all-day events and anything before 12:00 go in the morning
			if !ev.AllDay && when.Hour() >= 18 {
				i = 2
			} else if !ev.AllDay && when.Hour() >= 12 {
				i = 1
			}
			parts[i].Events = append(parts[i].Events, ev)
		case !when.Before(tomorrowStart) && when.Before(dayAfter) && len(tomorrow) < 3:
			tomorrow = append(tomorrow, ev)
		}
	}
	return parts, tomorrow
}

// degradedCards picks to-dos and reading without the LLM (doc 02 §7): actionable work items
// sorted by When, max 5; the newest 3 reading items.
func degradedCards(items []Item, card func([]string) Card) (todos, reading []Card) {
	actionable := map[string]bool{"pr_mine_ready": true, "pr_review_requested": true, "issue_assigned": true}
	var work, read []Item
	for _, it := range items {
		switch {
		case actionable[it.Kind]:
			work = append(work, it)
		case isReading(it.Source):
			read = append(read, it)
		}
	}
	slices.SortStableFunc(work, func(a, b Item) int { return a.When.Compare(b.When) })
	slices.SortStableFunc(read, func(a, b Item) int { return b.When.Compare(a.When) })
	for i, it := range work {
		if i == 5 {
			break
		}
		c := card([]string{it.ID})
		c.Label, c.Title, c.Body = sourceName(it.Source), it.Title, it.Body
		todos = append(todos, c)
	}
	for i, it := range read {
		if i == 3 {
			break
		}
		c := card([]string{it.ID})
		c.Title, c.Body = it.Title, it.Body
		reading = append(reading, c)
	}
	return todos, reading
}

func fallbackGreeting(items []Item, day time.Time, loc *time.Location) string {
	meetings, work := 0, 0
	for _, it := range items {
		switch {
		case it.Source == "calendar":
			if w := it.When.In(loc); !w.Before(day) && w.Before(day.AddDate(0, 0, 1)) {
				meetings++
			}
		case !isReading(it.Source):
			work++
		}
	}
	return fmt.Sprintf("%s today, %s open.", plural(meetings, "meeting"), plural(work, "work item"))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

//go:embed assets/brief.html.tmpl assets/brief.css assets/brief.js
var assets embed.FS

// The CSS and JS files are parsed as templates (not passed as data), so they are trusted
// template text and need no template.HTML/CSS/JS conversion. Data only ever goes through actions.
var pageTmpl = template.Must(template.New("brief.html.tmpl").
	Funcs(template.FuncMap{"join": strings.Join}).
	ParseFS(assets, "assets/brief.html.tmpl", "assets/brief.css", "assets/brief.js"))

func renderHTML(p Page) ([]byte, error) {
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return buf.Bytes(), nil
}
