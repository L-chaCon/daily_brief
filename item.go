package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Item is the universal currency of the pipeline (doc 02 §1). One flat struct for every source:
// a PR and a calendar event differ in which fields are filled, not in type.
type Item struct {
	ID     string     `json:"id"`
	Source string     `json:"source"`
	Kind   string     `json:"kind"`
	Title  string     `json:"title"`
	URL    string     `json:"url,omitempty"`
	Who    []string   `json:"who,omitempty"`
	When   time.Time  `json:"when"`
	End    *time.Time `json:"end,omitempty"` // calendar only
	Body   string     `json:"body,omitempty"`
	Facts  []string   `json:"facts,omitempty"`
}

// Raw is the output of collect: raw.json (doc 02 §4).
type Raw struct {
	Schema      int            `json:"schema"`
	Date        civilDate      `json:"date"`
	GeneratedAt time.Time      `json:"generated_at"`
	Timezone    string         `json:"timezone"`
	Sources     []SourceStatus `json:"sources"`
	Items       []Item         `json:"items"` // sorted: Source, When, ID
}

type SourceStatus struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"` // actionable: includes the fix command
	Count  int    `json:"count"`
	TookMS int64  `json:"took_ms"`
}

// readingSources are the non-work sources (doc 02 §3). Everything else is a work source.
var readingSources = map[string]bool{"youtube": true, "feed": true, "arxiv": true, "hn": true, "hfpapers": true}

func isReading(source string) bool { return readingSources[source] }

// Caps are in characters (runes), not bytes, so a title with accents or emoji isn't cut short.
const (
	maxTitle = 200
	maxBody  = 1500
)

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// Validate checks invariants 1–7 of doc 02 §1 and returns all violations joined, or nil.
// Invariant 6 (offset preserved) holds by construction once a time.Time is parsed, so it isn't checked here.
func (it Item) Validate() error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("item %q: %s", it.ID, fmt.Sprintf(format, args...)))
	}

	// 1. required fields
	if it.ID == "" || it.Source == "" || it.Kind == "" || it.Title == "" {
		bad("id, source, kind and title are required")
	}
	if it.When.IsZero() {
		bad("when is required")
	}
	// 2. id is namespaced by its source
	if it.Source != "" && !strings.HasPrefix(it.ID, it.Source+":") {
		bad("id must start with %q", it.Source+":")
	}
	// 3. one-line title, capped title and body
	if strings.ContainsAny(it.Title, "\r\n") {
		bad("title must be one line")
	}
	if n := utf8.RuneCountInString(it.Title); n > maxTitle {
		bad("title is %d chars, max %d", n, maxTitle)
	}
	if n := utf8.RuneCountInString(it.Body); n > maxBody {
		bad("body is %d chars, max %d", n, maxBody)
	}
	// 4. links are https only
	if it.URL != "" {
		u, err := url.Parse(it.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			bad("url must be an absolute https URL")
		}
	}
	// 5. End is set iff calendar, and never before When
	switch {
	case it.Source == "calendar" && it.End == nil:
		bad("calendar items need an end")
	case it.Source != "calendar" && it.End != nil:
		bad("only calendar items have an end")
	case it.End != nil && it.End.Before(it.When):
		bad("end is before when")
	}
	// 7. facts go to the LLM: no links, no email addresses
	for i, f := range it.Facts {
		if strings.Contains(f, "http://") || strings.Contains(f, "https://") || emailRe.MatchString(f) {
			bad("facts[%d] contains a link or an email address", i)
		}
	}
	return errors.Join(errs...)
}

// civilDate is a calendar date with no time and no time zone: "Wednesday's brief".
// Using time.Time at midnight invites midnight-in-which-zone bugs (doc 02 §4).
type civilDate struct {
	Y int
	M time.Month
	D int
}

func parseCivilDate(s string) (civilDate, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return civilDate{}, fmt.Errorf("date %q: want YYYY-MM-DD", s)
	}
	return civilDate{t.Year(), t.Month(), t.Day()}, nil
}

func (d civilDate) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Y, d.M, d.D) }

// In returns midnight at the start of d in loc.
func (d civilDate) In(loc *time.Location) time.Time { return time.Date(d.Y, d.M, d.D, 0, 0, 0, 0, loc) }

func (d civilDate) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *civilDate) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("date: want a JSON string: %w", err)
	}
	parsed, err := parseCivilDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
