package main

import (
	"strings"
	"testing"
)

func TestBriefRoundTrip(t *testing.T) {
	var b Brief
	roundTrip(t, basicDir+"/brief.json", &b)
}

// One case per rule V1–V10 (doc 02 §6). Each case starts from the valid fixture and breaks one thing,
// so the table reads as "this is what each rule forbids".
func TestBriefValidate(t *testing.T) {
	var raw Raw
	loadJSON(t, basicDir+"/raw.json", &raw)

	const (
		pr210   = "github:pr:acme/ops-app#210"
		pr215   = "github:pr:acme/ops-app#215"
		standup = "calendar:event:abc123@2026-10-14T14:00:00+01:00"
		article = "feed:entry:9c1e0b7a44d2"
	)
	tests := []struct {
		name    string
		edit    func(*Brief)
		wantErr []string // one substring per expected error, in order; nil = valid
	}{
		{"valid", func(b *Brief) {}, nil},
		{"valid: empty day with no focus", func(b *Brief) {
			b.Focus, b.ScheduleNotes, b.Reading = nil, nil, nil
		}, nil},
		{"V1 schema", func(b *Brief) { b.Schema = 2 }, []string{"V1"}},
		{"V2 unknown id", func(b *Brief) { b.Todos[0].ItemID = "github:pr:nope#1" }, []string{`V2: todos[0].item_id: unknown item "github:pr:nope#1"`}},
		{"V2 unknown id in focus", func(b *Brief) { b.Focus.ItemIDs = []string{"x:1"} }, []string{"V2: focus.item_ids[0]"}},
		{"V3 same item in todo and update", func(b *Brief) {
			b.Updates = append(b.Updates, Update{ItemIDs: []string{pr215}, Title: "t", Body: "b"})
		}, []string{"V3"}},
		{"V3 focus may overlap a todo", func(b *Brief) { b.Focus.ItemIDs = []string{pr210, pr215} }, nil},
		{"V4 note on a non-calendar item", func(b *Brief) { b.ScheduleNotes[0].ItemID = pr210 }, []string{"V4"}},
		{"V4 two notes on one event", func(b *Brief) {
			b.ScheduleNotes = append(b.ScheduleNotes, ScheduleNote{ItemID: standup, Note: "again"})
		}, []string{"V4"}},
		{"V5 reading pick from a work source", func(b *Brief) { b.Reading[0].ItemID = pr210 }, []string{"V5"}},
		{"V6 todo on a reading item", func(b *Brief) { b.Todos[1].ItemID = article }, []string{"V6"}},
		{"V6 focus on a reading item", func(b *Brief) { b.Focus.ItemIDs = []string{article} }, []string{"V6"}},
		{"V7 greeting too long", func(b *Brief) { b.Greeting = strings.Repeat("a", 301) }, []string{"V7: greeting"}},
		{"V7 label too long", func(b *Brief) { b.Todos[0].Label = strings.Repeat("a", 25) }, []string{"V7: todos[0].label"}},
		{"V7 too many todos", func(b *Brief) {
			for range 4 {
				b.Todos = append(b.Todos, b.Todos[0])
			}
		}, []string{"V7: 6 todos", "V3"}},
		{"V7 focus without ids", func(b *Brief) { b.Focus.ItemIDs = nil }, []string{"V7: focus.item_ids"}},
		{"V8 link in prose", func(b *Brief) { b.Todos[0].Body = "see https://evil.example" }, []string{"V8: todos[0].body"}},
		{"V8 link in a prompt", func(b *Brief) { b.Focus.Prompt = "open http://x.example" }, []string{"V8: focus.prompt"}},
		{"V9 html in prose", func(b *Brief) { b.Greeting = "hi <img src=x>" }, []string{"V9: greeting"}},
		{"V9 a lone < is fine", func(b *Brief) { b.Greeting = "fewer than 3 PRs, 2 < 3" }, nil},
		{"V10 empty brief on a busy day", func(b *Brief) { b.Todos, b.Updates = nil, nil }, []string{"V10"}},
		{"many problems are all reported", func(b *Brief) {
			b.Schema = 0
			b.Greeting = "<b>"
			b.Reading[0].ItemID = "nope"
		}, []string{"V1", "V2", "V9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b Brief
			loadJSON(t, basicDir+"/brief.json", &b) // fresh copy per case: edits mutate slices
			tt.edit(&b)
			errs := Validate(b, raw)
			if len(errs) != len(tt.wantErr) {
				t.Fatalf("got %d errors, want %d: %v", len(errs), len(tt.wantErr), errs)
			}
			for i, want := range tt.wantErr {
				if !strings.Contains(errs[i].Error(), want) {
					t.Errorf("error %d = %q, want it to contain %q", i, errs[i], want)
				}
			}
		})
	}
}
