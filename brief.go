package main

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Brief is the output of think: brief.json (doc 02 §5). The LLM picks items by id and writes
// short prose. It never outputs a URL, a time, or a handle; those are joined back from Raw.
type Brief struct {
	Schema        int            `json:"schema"`
	Greeting      string         `json:"greeting"`
	Focus         *Focus         `json:"focus"` // null on empty days
	Todos         []Todo         `json:"todos"`
	Updates       []Update       `json:"updates"`
	ScheduleNotes []ScheduleNote `json:"schedule_notes"`
	Reading       []Reading      `json:"reading"`
}

type Focus struct {
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	ItemIDs []string `json:"item_ids"`
	Prompt  string   `json:"prompt"`
}

type Todo struct {
	ItemID string `json:"item_id"`
	Label  string `json:"label"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

type Update struct {
	ItemIDs []string `json:"item_ids"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
}

type ScheduleNote struct {
	ItemID     string `json:"item_id"`
	Note       string `json:"note"`
	PrepPrompt string `json:"prep_prompt"`
}

type Reading struct {
	ItemID string `json:"item_id"`
	Why    string `json:"why"`
}

// Validate checks rules V1–V10 (doc 02 §6) and returns every problem, not just the first:
// the whole list is fed back to the LLM on its one retry. Each error starts with its rule id.
func Validate(b Brief, r Raw) []error {
	var errs []error
	bad := func(rule, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", rule, fmt.Sprintf(format, args...)))
	}
	items := make(map[string]Item, len(r.Items))
	for _, it := range r.Items {
		items[it.ID] = it
	}
	// lookup reports V2 for unknown ids, so the other rules only see real items.
	lookup := func(path, id string) (Item, bool) {
		it, ok := items[id]
		if !ok {
			bad("V2", "%s: unknown item %q", path, id)
		}
		return it, ok
	}

	// V1 contract version
	if b.Schema != 1 {
		bad("V1", "schema is %d, want 1", b.Schema)
	}

	// Focus: V2, V6, and the id count (V7)
	if f := b.Focus; f != nil {
		if n := len(f.ItemIDs); n < 1 || n > 6 {
			bad("V7", "focus.item_ids has %d ids, want 1–6", n)
		}
		for i, id := range f.ItemIDs {
			if it, ok := lookup(fmt.Sprintf("focus.item_ids[%d]", i), id); ok && isReading(it.Source) {
				bad("V6", "focus.item_ids[%d]: %q is a reading item, focus must be work", i, id)
			}
		}
	}

	// Todos and Updates: V2, V3, V6, counts (V7)
	if n := len(b.Todos); n > 5 {
		bad("V7", "%d todos, max 5", n)
	}
	if n := len(b.Updates); n > 5 {
		bad("V7", "%d updates, max 5", n)
	}
	cards := map[string]int{} // item id -> number of todo/update cards it appears in
	for i, t := range b.Todos {
		path := fmt.Sprintf("todos[%d].item_id", i)
		if it, ok := lookup(path, t.ItemID); ok && isReading(it.Source) {
			bad("V6", "%s: %q is a reading item, todos must be work", path, t.ItemID)
		}
		cards[t.ItemID]++
	}
	for i, u := range b.Updates {
		if n := len(u.ItemIDs); n < 1 || n > 3 {
			bad("V7", "updates[%d].item_ids has %d ids, want 1–3", i, n)
		}
		for j, id := range u.ItemIDs {
			lookup(fmt.Sprintf("updates[%d].item_ids[%d]", i, j), id)
			cards[id]++
		}
	}
	for _, id := range slices.Sorted(maps.Keys(cards)) { // sorted: same input, same error order
		if cards[id] > 1 {
			bad("V3", "item %q appears in %d todo/update cards, max 1", id, cards[id])
		}
	}

	// ScheduleNotes: V2, V4
	noted := map[string]bool{}
	for i, n := range b.ScheduleNotes {
		path := fmt.Sprintf("schedule_notes[%d].item_id", i)
		if it, ok := lookup(path, n.ItemID); ok && it.Source != "calendar" {
			bad("V4", "%s: %q is not a calendar item", path, n.ItemID)
		}
		if noted[n.ItemID] {
			bad("V4", "%s: %q already has a note", path, n.ItemID)
		}
		noted[n.ItemID] = true
	}

	// Reading: V2, V5, count (V7)
	if n := len(b.Reading); n > 3 {
		bad("V7", "%d reading picks, max 3", n)
	}
	for i, rd := range b.Reading {
		path := fmt.Sprintf("reading[%d].item_id", i)
		if it, ok := lookup(path, rd.ItemID); ok && !isReading(it.Source) {
			bad("V5", "%s: %q is not from a reading source", path, rd.ItemID)
		}
	}

	// V7 length caps, V8 no links, V9 no HTML, over every prose field
	for _, t := range b.texts() {
		if n := utf8.RuneCountInString(t.s); n > t.max {
			bad("V7", "%s is %d chars, max %d", t.path, n, t.max)
		}
		if strings.Contains(t.s, "http://") || strings.Contains(t.s, "https://") {
			bad("V8", "%s contains a link; links come from raw items only", t.path)
		}
		if htmlTagRe.MatchString(t.s) {
			bad("V9", "%s looks like it contains HTML", t.path)
		}
	}

	// V10 an empty brief on a busy day is a failure
	if len(b.Todos)+len(b.Updates) == 0 {
		for _, it := range r.Items {
			if !isReading(it.Source) {
				bad("V10", "raw has work items but the brief has no todos or updates")
				break
			}
		}
	}
	return errs
}

var htmlTagRe = regexp.MustCompile(`<[A-Za-z]`)

type text struct {
	path string
	s    string
	max  int
}

// texts lists every prose field with its cap (doc 02 §5), so V7–V9 are one loop.
func (b Brief) texts() []text {
	ts := []text{{"greeting", b.Greeting, 300}}
	if f := b.Focus; f != nil {
		ts = append(ts, text{"focus.title", f.Title, 80}, text{"focus.body", f.Body, 600}, text{"focus.prompt", f.Prompt, 500})
	}
	for i, t := range b.Todos {
		p := fmt.Sprintf("todos[%d].", i)
		ts = append(ts, text{p + "label", t.Label, 24}, text{p + "title", t.Title, 80}, text{p + "body", t.Body, 300})
	}
	for i, u := range b.Updates {
		p := fmt.Sprintf("updates[%d].", i)
		ts = append(ts, text{p + "title", u.Title, 80}, text{p + "body", u.Body, 400})
	}
	for i, n := range b.ScheduleNotes {
		p := fmt.Sprintf("schedule_notes[%d].", i)
		ts = append(ts, text{p + "note", n.Note, 200}, text{p + "prep_prompt", n.PrepPrompt, 300})
	}
	for i, r := range b.Reading {
		ts = append(ts, text{fmt.Sprintf("reading[%d].why", i), r.Why, 200})
	}
	return ts
}
