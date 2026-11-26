package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// source is one row of the source table. Adding a source = one function + one line here.
type source struct {
	name    string
	collect func(ctx context.Context, env Env) ([]Item, error)
	timeout time.Duration // 0 = sourceTimeout
}

var sources = []source{
	{"github", collectGitHub, 0},
	{"jira", collectJira, 60 * time.Second},         // one acli call per issue
	{"slack", collectSlack, 90 * time.Second},       // claude -p + connector connect, ≤ 3 search pages
	{"calendar", collectCalendar, 90 * time.Second}, // claude transport; icalBuddy later (same decode→build boundary)
}

const sourceTimeout = 20 * time.Second

// collectAll never fails as a whole: each source's outcome lands in its SourceStatus.
// Sequential on purpose (doc 01 §7.3); concurrency is a later, measured change.
func collectAll(ctx context.Context, env Env, date civilDate, table []source) Raw {
	raw := Raw{Schema: 1, Date: date, GeneratedAt: env.Now, Timezone: env.Loc.String(), Items: []Item{}, Sources: []SourceStatus{}}
	seen := map[string]bool{}
	for _, s := range table {
		if slices.Contains(env.Config.Disabled, s.name) {
			continue
		}
		items, st := runOne(ctx, env, s)
		kept := 0
		for _, it := range items {
			if seen[it.ID] { // a duplicate id is a collector bug: keep the first, count the rest
				st.Dropped++
				continue
			}
			seen[it.ID] = true
			raw.Items = append(raw.Items, it)
			kept++
		}
		st.Count = kept
		raw.Sources = append(raw.Sources, st.SourceStatus)
		if st.Dropped > 0 {
			raw.Sources[len(raw.Sources)-1].Error = joinNonEmpty(st.Error, fmt.Sprintf("%d invalid or duplicate items dropped", st.Dropped))
		}
	}
	// Sorting is part of the contract: same input, same file, same prompt (doc 02 §4).
	slices.SortFunc(raw.Items, func(a, b Item) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), a.When.Compare(b.When), cmp.Compare(a.ID, b.ID))
	})
	return raw
}

type runResult struct {
	SourceStatus
	Dropped int
}

// runOne runs one collector with a timeout and a recover, then validates every item.
// Invalid items are dropped and counted, not fatal: one bad ticket must not cost the brief.
func runOne(ctx context.Context, env Env, s source) (items []Item, st runResult) {
	st.Name = s.name
	start := time.Now()
	defer func() {
		st.TookMS = time.Since(start).Milliseconds()
		if r := recover(); r != nil {
			items, st.OK, st.Error = nil, false, fmt.Sprintf("%s: panic: %v (a bug in the collector)", s.name, r)
		}
	}()
	timeout := cmp.Or(s.timeout, sourceTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	got, err := s.collect(ctx, env)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("%s: timeout after %s", s.name, timeout)
		}
		st.Error = err.Error()
		if len(got) == 0 {
			return nil, st
		}
	}
	st.OK = true // partial results still count: the error stays visible in the footer
	for _, it := range got {
		if it.Validate() != nil {
			st.Dropped++
			continue
		}
		items = append(items, it)
	}
	return items, st
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
