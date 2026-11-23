package main

import (
	"bytes"
	"strings"
	"testing"
)

// Mission 1 spec. Written for you; you make it pass without editing it.
// Read it top to bottom before writing main.go: it tells you exactly what run() must do.

var commands = []string{"collect", "think", "render", "run", "doctor", "connect", "open"}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string // substring that must appear in stdout ("" = stdout must be empty)
		wantStderr string // substring that must appear in stderr ("" = stderr must be empty)
	}{
		{"no args prints usage to stderr", nil, 2, "", "usage:"},
		{"-h prints usage to stdout", []string{"-h"}, 0, "usage:", ""},
		{"--help prints usage to stdout", []string{"--help"}, 0, "usage:", ""},
		{"help prints usage to stdout", []string{"help"}, 0, "usage:", ""},
		{"unknown command", []string{"bogus"}, 2, "", `unknown command "bogus"`},
		{"known command is a stub", []string{"collect"}, 2, "", "collect: not implemented"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			check(t, "stdout", stdout.String(), tt.wantStdout)
			check(t, "stderr", stderr.String(), tt.wantStderr)
		})
	}
}

// Usage must list every subcommand, so a user (or an AI agent) can discover them.
func TestUsageListsAllCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"-h"}, &stdout, &stderr)
	for _, c := range commands {
		if !strings.Contains(stdout.String(), c) {
			t.Errorf("usage does not mention %q:\n%s", c, stdout.String())
		}
	}
}

// Every subcommand exists (as a stub for now) and none of them is "unknown".
func TestAllCommandsAreKnown(t *testing.T) {
	for _, c := range commands {
		var stdout, stderr bytes.Buffer
		code := run([]string{c}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), c+": not implemented") {
			t.Errorf("%s: code=%d stderr=%q, want 2 and %q", c, code, stderr.String(), c+": not implemented")
		}
	}
}

func check(t *testing.T, stream, got, want string) {
	t.Helper()
	if want == "" && got != "" {
		t.Errorf("%s should be empty, got %q", stream, got)
	}
	if want != "" && !strings.Contains(got, want) {
		t.Errorf("%s = %q, want it to contain %q", stream, got, want)
	}
}
