package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	raw, _ := loadBasic(t)
	system, user, err := buildPrompt(raw, Config{Interests: []string{"agents", "Go"}})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "testdata/think/basic.prompt.golden.txt", []byte(system+"\n---\n"+user))

	if strings.Contains(user, "http") {
		t.Error("the user message must contain no URLs: the model can't copy what it never saw")
	}
	for _, want := range []string{"Wednesday, 2026-10-14", "agents, Go", "mail FAILED", `"day":"tomorrow","when":"09:30–10:30"`} {
		if !strings.Contains(system+user, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// Work first (review requested before ready), then calendar, then reading.
	order := []string{"#215", "#210", "OPS-1452", "k3j9", "abc123", "p1an", "feed:entry"}
	last := -1
	for _, id := range order {
		i := strings.Index(user, id)
		if i < last {
			t.Errorf("%s is out of order", id)
		}
		last = i
	}
}

func TestBuildPromptPrivacy(t *testing.T) {
	raw, _ := loadBasic(t)
	raw.Items[4].Facts = append(raw.Items[4].Facts, "from: sam@example.com") // invalid: email in facts
	_, user, err := buildPrompt(raw, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(user, "sam@example.com") || strings.Contains(user, "#210") {
		t.Error("an invalid item reached the prompt")
	}
	if !strings.Contains(user, "NOTE: 1 invalid items omitted") {
		t.Error("dropping must be visible in the prompt")
	}
}

// fakeThinker returns the given answers in order and counts calls.
func fakeThinker(t *testing.T, answers ...string) (Thinker, *int, *[]string) {
	calls, users := 0, []string{}
	return func(_ context.Context, _, user string) (json.RawMessage, error) {
		users = append(users, user)
		if calls >= len(answers) {
			t.Fatalf("unexpected call %d", calls+1)
		}
		calls++
		if strings.HasPrefix(answers[calls-1], "ERR:") {
			return nil, errors.New(answers[calls-1])
		}
		return json.RawMessage(answers[calls-1]), nil
	}, &calls, &users
}

func TestThink(t *testing.T) {
	raw, _ := loadBasic(t)
	valid, _ := os.ReadFile(basicDir + "/brief.json")
	invalid := `{"schema":1,"greeting":"hi","focus":null,"todos":[{"item_id":"github:pr:nope#1","label":"x","title":"t","body":"b"}],"updates":[],"schedule_notes":[],"reading":[]}`

	t.Run("valid first time", func(t *testing.T) {
		llm, calls, _ := fakeThinker(t, string(valid))
		b, err := think(context.Background(), raw, Config{}, llm)
		if err != nil || b == nil || *calls != 1 {
			t.Fatalf("b=%v err=%v calls=%d", b, err, *calls)
		}
	})
	t.Run("invalid then valid: one retry with the problems listed", func(t *testing.T) {
		llm, calls, users := fakeThinker(t, invalid, string(valid))
		if _, err := think(context.Background(), raw, Config{}, llm); err != nil || *calls != 2 {
			t.Fatalf("err=%v calls=%d", err, *calls)
		}
		if !strings.Contains((*users)[1], "YOUR PREVIOUS ANSWER WAS INVALID:\n- V2: todos[0].item_id") {
			t.Errorf("retry message doesn't list the problem:\n%s", (*users)[1])
		}
	})
	t.Run("invalid twice: error lists all problems", func(t *testing.T) {
		llm, calls, _ := fakeThinker(t, invalid, `{"schema":2}`)
		_, err := think(context.Background(), raw, Config{}, llm)
		if err == nil || *calls != 2 || !strings.Contains(err.Error(), "invalid after retry") || !strings.Contains(err.Error(), "V1") {
			t.Fatalf("err=%v calls=%d", err, *calls)
		}
	})
	t.Run("CLI error is not retried", func(t *testing.T) {
		llm, calls, _ := fakeThinker(t, "ERR: claude: not logged in")
		if _, err := think(context.Background(), raw, Config{}, llm); err == nil || *calls != 1 {
			t.Fatalf("err=%v calls=%d", err, *calls)
		}
	})
	t.Run("unknown field is a validation problem, not a crash", func(t *testing.T) {
		llm, calls, _ := fakeThinker(t, `{"schema":1,"extra":true}`, string(valid))
		if _, err := think(context.Background(), raw, Config{}, llm); err != nil || *calls != 2 {
			t.Fatalf("err=%v calls=%d", err, *calls)
		}
	})
}

func TestThinkDir(t *testing.T) {
	env := testEnv(t)
	dir := t.TempDir()
	b, _ := os.ReadFile(basicDir + "/raw.json")
	os.WriteFile(filepath.Join(dir, "raw.json"), b, 0o644)
	os.WriteFile(filepath.Join(dir, "brief.json"), []byte("stale"), 0o644)

	// dry run: prompt only, zero model calls
	llm, calls, _ := fakeThinker(t)
	if err := thinkDir(context.Background(), env, dir, llm, true); err != nil || *calls != 0 {
		t.Fatalf("err=%v calls=%d", err, *calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "prompt.txt")); err != nil {
		t.Error("dry run must write prompt.txt")
	}

	// failure: the stale brief.json is gone and think.error.txt explains why
	llm, _, _ = fakeThinker(t, "ERR: claude: boom")
	if err := thinkDir(context.Background(), env, dir, llm, false); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(filepath.Join(dir, "brief.json")); err == nil {
		t.Error("stale brief.json survived a failed think")
	}
	if _, err := loadBrief(dir, Raw{}); err == nil || !strings.Contains(err.Error(), "claude: boom") {
		t.Errorf("render should explain the failure, got %v", err)
	}
}

func TestParseEnvelope(t *testing.T) {
	ok := `{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"schema":1},"num_turns":2,"total_cost_usd":0.01}`
	if out, err := parseEnvelope([]byte(ok)); err != nil || string(out) != `{"schema":1}` {
		t.Errorf("out=%s err=%v", out, err)
	}
	for _, bad := range []string{
		`{"subtype":"error_max_turns","is_error":true,"result":"too many turns"}`,
		`{"subtype":"success","is_error":false,"structured_output":null}`,
		`not json`,
	} {
		if _, err := parseEnvelope([]byte(bad)); err == nil {
			t.Errorf("want error for %s", bad)
		}
	}
}

// Cheap drift detector: the schema sent to the model and the Go structs agree on field names.
func TestSchemaMatchesStructs(t *testing.T) {
	var schema struct {
		Required   []string
		Properties map[string]struct {
			Items struct {
				Required []string
			}
			AnyOf []struct{ Required []string }
		}
	}
	if err := json.Unmarshal(briefSchema, &schema); err != nil {
		t.Fatal(err)
	}
	check := func(name string, typ reflect.Type, required []string) {
		var tags []string
		for f := range typ.Fields() {
			tags = append(tags, strings.Split(f.Tag.Get("json"), ",")[0])
		}
		if !reflect.DeepEqual(tags, required) {
			t.Errorf("%s: struct tags %v, schema required %v", name, tags, required)
		}
	}
	check("Brief", reflect.TypeFor[Brief](), schema.Required)
	check("Focus", reflect.TypeFor[Focus](), schema.Properties["focus"].AnyOf[1].Required)
	check("Todo", reflect.TypeFor[Todo](), schema.Properties["todos"].Items.Required)
	check("Update", reflect.TypeFor[Update](), schema.Properties["updates"].Items.Required)
	check("ScheduleNote", reflect.TypeFor[ScheduleNote](), schema.Properties["schedule_notes"].Items.Required)
	check("Reading", reflect.TypeFor[Reading](), schema.Properties["reading"].Items.Required)
}

func TestPromptStripsLinksFromBodies(t *testing.T) {
	raw, _ := loadBasic(t)
	raw.Items[5].Body = "see https://example.com/x and http://y.example"
	_, user, err := buildPrompt(raw, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(user, "http") || !strings.Contains(user, "see [link] and [link]") {
		t.Errorf("links must not reach the model")
	}
}
