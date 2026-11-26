package main

import (
	"bytes"
	"strings"
	"testing"
)

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

func check(t *testing.T, stream, got, want string) {
	t.Helper()
	if want == "" && got != "" {
		t.Errorf("%s should be empty, got %q", stream, got)
	}
	if want != "" && !strings.Contains(got, want) {
		t.Errorf("%s = %q, want it to contain %q", stream, got, want)
	}
}
