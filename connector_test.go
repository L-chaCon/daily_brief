package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const slackSearchTool = "mcp__claude_ai_Slack__slack_search_public_and_private"

var streamArgs = map[string]any{"filters": "to:me after:2026-10-12", "include_context": false, "limit": 20}

func TestToolResult(t *testing.T) {
	stream, err := os.ReadFile("testdata/claude/stream_one_call.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	got, err := toolResult(stream, slackSearchTool, streamArgs)
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Results string }
	if err := json.Unmarshal(got, &v); err != nil || !strings.Contains(v.Results, "Search Results") {
		t.Errorf("tool result = %q, %v", got, err)
	}

	// The model must not change what Go asked for: different arguments = no data.
	changed := map[string]any{"filters": "in:#general", "include_context": false, "limit": 20}
	if _, err := toolResult(stream, slackSearchTool, changed); err == nil || !strings.Contains(err.Error(), "changed the arguments") {
		t.Errorf("args mismatch: want error, got %v", err)
	}

	// No call to the tool at all: the connector isn't there (wrong account, not connected).
	noCall := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"I have no Slack tool."}]}}
{"type":"result","subtype":"success","is_error":false,"result":"I have no Slack tool."}
`)
	if _, err := toolResult(noCall, slackSearchTool, streamArgs); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("no call: want 'not available', got %v", err)
	}

	// The tool itself failed: its message is the error. String content, as some connectors send.
	failed := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"` + slackSearchTool + `","input":{"filters":"to:me after:2026-10-12","include_context":false,"limit":20}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"invalid_auth: token revoked\nmore detail"}]}}
`)
	if _, err := toolResult(failed, slackSearchTool, streamArgs); err == nil || !strings.Contains(err.Error(), "invalid_auth: token revoked") || strings.Contains(err.Error(), "more detail") {
		t.Errorf("tool error: got %v", err)
	}

	// Anything beyond the one call (a second call, another tool, a denied call) = the data steered the
	// model. The page is not used, even though the first result looks fine.
	good := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"` + slackSearchTool + `","input":{"filters":"to:me after:2026-10-12","include_context":false,"limit":20}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"{}"}]}}
`
	for name, tail := range map[string]string{
		"second call": `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"` + slackSearchTool + `","input":{"filters":"in:#secret"}}]}}` + "\n",
		"other tool":  `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"mcp__claude_ai_Slack__slack_send_message","input":{}}]}}` + "\n",
		"denied":      `{"type":"result","subtype":"success","is_error":false,"result":"done","permission_denials":[{"tool_name":"mcp__claude_ai_Slack__slack_send_message"}]}` + "\n",
	} {
		if _, err := toolResult([]byte(good+tail), slackSearchTool, streamArgs); err == nil || !strings.Contains(err.Error(), "prompt injection") {
			t.Errorf("%s: want injection error, got %v", name, err)
		}
	}
	if _, err := toolResult([]byte(good), slackSearchTool, streamArgs); err != nil {
		t.Errorf("one clean call: %v", err)
	}

	// A result too big for claude's context is replaced by a note pointing at a file: an error, not data.
	tooBig := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"` + slackSearchTool + `","input":{"filters":"to:me after:2026-10-12","include_context":false,"limit":20}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"Error: result (312,004 characters) exceeds maximum allowed tokens. Output has been saved to /tmp/fake.txt.\nFormat: JSON"}]}}
`
	if _, err := toolResult([]byte(tooBig), slackSearchTool, streamArgs); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("too big: got %v", err)
	}

	// claude itself failed before any tool call (e.g. not logged in).
	notLogged := []byte(`{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key · Please run /login"}` + "\n")
	if _, err := toolResult(notLogged, slackSearchTool, streamArgs); err == nil || !strings.Contains(err.Error(), "/login") {
		t.Errorf("claude error: got %v", err)
	}
}

func TestCallConnector(t *testing.T) {
	env := testEnv(t)
	env.Config.ClaudeConfigDir = "/tmp/fake-claude-work"
	env.RunDir = t.TempDir()
	stream, _ := os.ReadFile("testdata/claude/stream_one_call.jsonl")
	var name string
	var argv []string
	env.Run = func(_ context.Context, n string, args ...string) ([]byte, error) {
		name, argv = n, args
		return stream, nil
	}
	if _, err := callConnector(context.Background(), env, "slack", slackSearchTool, streamArgs); err != nil {
		t.Fatal(err)
	}
	if name != "env" {
		t.Fatalf("runs %q, want env (to set claude's environment)", name)
	}
	for _, want := range []string{"MCP_CONNECTION_NONBLOCKING=false", "CLAUDE_CONFIG_DIR=/tmp/fake-claude-work", "claude", "-p",
		"--allowedTools", slackSearchTool, "--disallowedTools", "--output-format", "stream-json", "--verbose", "--setting-sources", "--no-session-persistence"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv missing %q: %v", want, argv)
		}
	}
	// --strict-mcp-config drops claude.ai connectors (D23): it must not be there.
	if slices.Contains(argv, "--strict-mcp-config") {
		t.Error("argv has --strict-mcp-config, which hides claude.ai connectors")
	}
	// --tools "" removes every built-in tool (Bash, Write, WebFetch, …).
	if i := slices.Index(argv, "--tools"); i < 0 || argv[i+1] != "" {
		t.Errorf("want --tools \"\": %v", argv)
	}
	// The prompt comes right after -p: --disallowedTools is variadic and would swallow a trailing prompt.
	if i := slices.Index(argv, "-p"); i < 0 || !strings.Contains(argv[i+1], slackSearchTool) {
		t.Errorf("prompt must follow -p: %v", argv)
	}
	// Every write tool of every connector is denied, and other servers are denied whole.
	deny := strings.Join(argv, " ")
	for _, w := range []string{"slack_send_message", "slack_schedule_message", "slack_add_reaction", "create_event", "delete_event", "mcp__claude_ai_Gmail"} {
		if !strings.Contains(deny, w) {
			t.Errorf("not denied: %s", w)
		}
	}
	if strings.Contains(deny, " mcp__claude_ai_Slack ") {
		t.Error("the tool's own server must not be denied whole")
	}
	// The transcript is kept in the run folder, so a run can be inspected.
	if _, err := callConnector(context.Background(), env, "slack", slackSearchTool, streamArgs); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(env.RunDir, "slack.claude.jsonl"))
	if err != nil || string(saved) != string(stream)+string(stream) {
		t.Errorf("transcript: want both calls appended (pages), got %d bytes, %v", len(saved), err)
	}

	// Without a configured account, claude's own default is inherited (D18): no CLAUDE_CONFIG_DIR forced.
	env.Config.ClaudeConfigDir = ""
	_, _ = callConnector(context.Background(), env, "slack", slackSearchTool, streamArgs)
	for _, a := range argv {
		if strings.HasPrefix(a, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("forced %s with no config", a)
		}
	}

	env.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 127: env: claude: No such file or directory")
	}
	if _, err := callConnector(context.Background(), env, "slack", slackSearchTool, streamArgs); err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("want PATH hint, got %v", err)
	}
}
