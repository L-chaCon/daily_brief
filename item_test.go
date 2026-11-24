package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const basicDir = "testdata/render/basic"

func TestItemValidate(t *testing.T) {
	when := time.Date(2026, 10, 14, 14, 0, 0, 0, time.FixedZone("BST", 3600))
	end := when.Add(15 * time.Minute)
	before := when.Add(-time.Minute)
	pr := Item{ID: "github:pr:acme/ops-app#210", Source: "github", Kind: "pr_mine_ready",
		Title: "Package count", URL: "https://github.com/acme/ops-app/pull/210", When: when}
	ev := Item{ID: "calendar:event:abc@x", Source: "calendar", Kind: "event", Title: "Standup", When: when, End: &end}

	tests := []struct {
		name    string
		item    Item
		edit    func(*Item)
		wantErr string // substring; "" = valid
	}{
		{"valid pr", pr, nil, ""},
		{"valid event", ev, nil, ""},
		{"1 missing title", pr, func(it *Item) { it.Title = "" }, "required"},
		{"1 zero when", pr, func(it *Item) { it.When = time.Time{} }, "when is required"},
		{"2 id not namespaced", pr, func(it *Item) { it.ID = "pr:210" }, `start with "github:"`},
		{"3 title newline", pr, func(it *Item) { it.Title = "a\nb" }, "one line"},
		{"3 title too long", pr, func(it *Item) { it.Title = strings.Repeat("a", 201) }, "max 200"},
		{"3 title cap counts chars not bytes", pr, func(it *Item) { it.Title = strings.Repeat("é", 200) }, ""},
		{"3 body too long", pr, func(it *Item) { it.Body = strings.Repeat("a", 1501) }, "max 1500"},
		{"4 http url", pr, func(it *Item) { it.URL = "http://example.com" }, "https"},
		{"4 javascript url", pr, func(it *Item) { it.URL = "javascript:alert(1)" }, "https"},
		{"5 calendar without end", ev, func(it *Item) { it.End = nil }, "need an end"},
		{"5 end on non-calendar", pr, func(it *Item) { it.End = &end }, "only calendar"},
		{"5 end before when", ev, func(it *Item) { it.End = &before }, "before when"},
		{"7 link in facts", pr, func(it *Item) { it.Facts = []string{"see https://x.com"} }, "facts[0]"},
		{"7 email in facts", pr, func(it *Item) { it.Facts = []string{"from: sam@example.com"} }, "facts[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := tt.item
			if tt.edit != nil {
				tt.edit(&it)
			}
			err := it.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("want valid, got %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestFixtureItemsAreValid(t *testing.T) {
	var r Raw
	loadJSON(t, basicDir+"/raw.json", &r)
	for _, it := range r.Items {
		if err := it.Validate(); err != nil {
			t.Error(err)
		}
	}
}

func TestCivilDate(t *testing.T) {
	if got := (civilDate{2026, time.January, 5}).String(); got != "2026-01-05" {
		t.Errorf("String() = %q, want zero-padded 2026-01-05", got)
	}
	b, err := json.Marshal(civilDate{2026, time.October, 14})
	if err != nil || string(b) != `"2026-10-14"` {
		t.Errorf("Marshal = %s, %v; want \"2026-10-14\"", b, err)
	}
	for _, in := range []string{`"2026-13-01"`, `"2026-02-30"`, `"14/10/2026"`, `"2026-10-14T08:00:00Z"`, `20261014`} {
		var d civilDate
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("Unmarshal(%s) = %v, want error", in, d)
		}
	}
}

// Round trip: file -> struct -> file is byte-identical. Catches missing fields, wrong tags,
// wrong field order and wrong omitempty in one go (doc 08 §3).
func TestRawRoundTrip(t *testing.T) {
	var r Raw
	roundTrip(t, basicDir+"/raw.json", &r)
	if r.Date != (civilDate{2026, time.October, 14}) {
		t.Errorf("Date = %v", r.Date)
	}
}

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func roundTrip(t *testing.T, path string, v any) {
	t.Helper()
	loadJSON(t, path, v)
	want, _ := os.ReadFile(path)
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.TrimRight(want, "\n")) {
		t.Errorf("round trip of %s changed the bytes.\n--- got\n%s", path, got)
	}
}
