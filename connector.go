package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// claude.ai connectors (D23, D24): the work Claude account has company-installed connectors
// (Slack, Google Calendar, …). claude -p is only the transport: Go names the tool and its arguments,
// the model makes that one call, and Go reads the tool's own output from the transcript.
// The model writes no facts, and it only sees untrusted text after its single call.

const connectorModel = "haiku" // making one prescribed call needs no judgment

// connectorServers are the claude.ai connectors on the account. All but the called tool's own
// server are denied whole: fewer tool schemas in context (cost), nothing else reachable.
var connectorServers = []string{
	"mcp__claude_ai_Slack", "mcp__claude_ai_Google_Calendar", "mcp__claude_ai_Gmail",
	"mcp__claude_ai_Google_Drive", "mcp__claude_ai_Google_Docs", "mcp__claude_ai_Atlassian_MCP",
	"mcp__claude_ai_Meetings", "mcp__claude_ai_Claude_Data", "mcp__claude_ai_Claude_Docs",
}

// connectorWriteTools are denied always. -p already denies any tool not in --allowedTools
// (nobody can approve it): that is the real fence, and covers connectors or write tools added after
// this list was written. This list is the second fence. Read-only: brief never acts for you.
var connectorWriteTools = []string{
	"mcp__claude_ai_Slack__slack_send_message", "mcp__claude_ai_Slack__slack_send_message_draft",
	"mcp__claude_ai_Slack__slack_schedule_message", "mcp__claude_ai_Slack__slack_add_reaction",
	"mcp__claude_ai_Slack__slack_create_canvas", "mcp__claude_ai_Slack__slack_update_canvas",
	"mcp__claude_ai_Slack__slack_create_conversation", "mcp__claude_ai_Slack__slack_create_list",
	"mcp__claude_ai_Slack__slack_update_list", "mcp__claude_ai_Slack__slack_add_list_record",
	"mcp__claude_ai_Slack__slack_update_list_record", "mcp__claude_ai_Slack__slack_add_list_comment",
	"mcp__claude_ai_Slack__slack_update_list_comment", "mcp__claude_ai_Slack__slack_get_file_upload_url",
	"mcp__claude_ai_Slack__slack_complete_file_upload",
	"mcp__claude_ai_Google_Calendar__create_event", "mcp__claude_ai_Google_Calendar__update_event",
	"mcp__claude_ai_Google_Calendar__delete_event", "mcp__claude_ai_Google_Calendar__respond_to_event",
}

// callConnector asks claude to make exactly one call to tool with args, and returns the tool's raw
// output. The transcript is saved as <source>.claude.jsonl in the run folder.
func callConnector(ctx context.Context, env Env, source, tool string, args any) ([]byte, error) {
	argJSON, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	prompt := fmt.Sprintf("Call %s exactly once with exactly these arguments: %s. Then reply: done.", tool, argJSON)

	server, _, _ := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__")
	server = "mcp__" + server
	var deny []string
	for _, s := range connectorServers {
		if s != server {
			deny = append(deny, s)
		}
	}
	deny = append(deny, connectorWriteTools...)

	// `env` sets claude's environment through env.Run, so tests fake it like any other command.
	// MCP_CONNECTION_NONBLOCKING=false: in -p mode connectors connect async and the model would start
	// without them. No --strict-mcp-config: it drops claude.ai connectors (doc 03 §6).
	argv := []string{"MCP_CONNECTION_NONBLOCKING=false"}
	if d := env.Config.ClaudeConfigDir; d != "" {
		argv = append(argv, "CLAUDE_CONFIG_DIR="+expandHome(d))
	}
	argv = append(argv, "claude", "-p", prompt, // prompt right after -p: --disallowedTools is variadic
		"--model", connectorModel,
		"--tools", "", "--setting-sources", "", "--disable-slash-commands", "--no-session-persistence",
		"--max-turns", "2",
		"--output-format", "stream-json", "--verbose",
		"--allowedTools", tool,
		"--disallowedTools")
	argv = append(argv, deny...)

	out, runErr := env.Run(ctx, "env", argv...)
	if env.RunDir != "" && len(out) > 0 { // appended: a source may call several times (pages); collect clears it first
		if f, err := os.OpenFile(filepath.Join(env.RunDir, source+".claude.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.Write(out)
			_ = f.Close()
		}
	}
	if runErr != nil && len(out) == 0 {
		if strings.Contains(runErr.Error(), "claude: No such file") {
			return nil, fmt.Errorf("%s: claude not found on PATH. Install Claude Code, or set PATH in the launchd plist", source)
		}
		return nil, fmt.Errorf("%s: claude: %w", source, runErr)
	}
	res, err := toolResult(out, tool, args)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return res, nil
}

type streamEvent struct {
	Type              string            `json:"type"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Message           struct {
		Content []streamBlock `json:"content"`
	} `json:"message"`
}

type streamBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"` // a string, or [{type: text, text}]
}

// toolResult finds the call to tool in a stream-json transcript, checks the model used exactly args,
// and returns that call's output. Pure: the tests feed it hand-written transcripts.
//
// The whole transcript is read before anything is returned: any tool call after the first one, or any
// denied call, means the model did something it wasn't asked to (likely text in the result steering it).
// Then the page is not used at all. Fail loud: the brief shows the source error.
func toolResult(stream []byte, tool string, args any) ([]byte, error) {
	want, err := normJSON(args)
	if err != nil {
		return nil, err
	}
	var (
		callID, final string
		finalErr      bool
		result        []byte
		resultErr     error
		extra         []string // tool calls after the first
		denied        int
	)
	for line := range bytes.Lines(stream) {
		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil {
			continue // not every line is an event we know; the ones we need are checked below
		}
		if ev.Type == "result" {
			final, finalErr, denied = ev.Result, ev.IsError, len(ev.PermissionDenials)
		}
		for _, b := range ev.Message.Content {
			switch {
			case ev.Type == "assistant" && b.Type == "tool_use" && callID != "":
				extra = append(extra, shortTool(b.Name))
			case ev.Type == "assistant" && b.Type == "tool_use":
				if b.Name != tool {
					extra = append(extra, shortTool(b.Name))
					continue
				}
				got, err := normJSON(b.Input)
				if err != nil || !reflect.DeepEqual(got, want) {
					return nil, fmt.Errorf("claude changed the arguments: asked %s, it sent %s", mustJSON(args), b.Input)
				}
				callID = b.ID
			case ev.Type == "user" && b.Type == "tool_result" && b.ToolUseID == callID && callID != "":
				text := blockText(b.Content)
				if strings.HasPrefix(text, "Error: result (") && strings.Contains(text, "exceeds maximum allowed tokens") {
					// claude saved the output to a file and gave the model a note instead: never parse the note.
					first, _, _ := strings.Cut(text, ".")
					resultErr = fmt.Errorf("%s result too large for claude (%s); ask for a smaller page", shortTool(tool), strings.TrimPrefix(first, "Error: "))
				} else if b.IsError {
					first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
					resultErr = fmt.Errorf("%s failed: %s", shortTool(tool), first)
				}
				result = []byte(text)
			}
		}
	}
	if len(extra) > 0 || denied > 0 {
		return nil, fmt.Errorf("claude did more than the one call it was asked for (extra calls: %v, denied: %d); "+
			"possible prompt injection in the data, this result is not used. See the transcript in the run folder", extra, denied)
	}
	switch {
	case resultErr != nil:
		return nil, resultErr
	case result != nil:
		return result, nil
	case finalErr:
		first, _, _ := strings.Cut(strings.TrimSpace(final), "\n")
		return nil, fmt.Errorf("claude: %s", first)
	case callID != "":
		return nil, fmt.Errorf("%s was called but returned nothing", shortTool(tool))
	}
	return nil, errors.New("connector tool " + shortTool(tool) + " not available on this Claude account. " +
		"Check the claude.ai connector is connected, and claude_config_dir in ~/.config/brief/config.json")
}

// blockText returns a tool_result's text, whether it came as a string or as text blocks.
func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct{ Type, Text string }
	_ = json.Unmarshal(raw, &blocks)
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// normJSON round-trips v through JSON so Go values and decoded tool input compare equal.
func normJSON(v any) (any, error) {
	b, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if b, err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	var out any
	return out, json.Unmarshal(b, &out)
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func shortTool(tool string) string { return tool[strings.LastIndex(tool, "__")+2:] }
