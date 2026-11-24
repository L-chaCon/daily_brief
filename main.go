package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const usage = `usage: brief <command> [flags]

commands:
  collect   read every source into raw.json
  think     ask the LLM to pick and write brief.json
  render    join raw.json + brief.json into brief.html
  run       collect, think, render, open
  doctor    check that every source is connected
  connect   store a source's secret in the Keychain
  open      open a day's brief in the browser
  help      show this help`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "-h", "help", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "render":
		return cmdRender(rest, stdout, stderr)
	case "collect", "think", "run", "doctor", "connect", "open":
		fmt.Fprintf(stderr, "%s: not implemented\n", cmd)
		return 2
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", cmd)
		return 2
	}
}

// cmdRender reads <dir>/raw.json and, unless --no-think, <dir>/brief.json, and writes <dir>/brief.html.
// A missing or invalid brief.json is not an error: the page renders degraded and says why.
func cmdRender(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("render", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dir := fl.String("dir", "", "run folder containing raw.json (and brief.json)")
	noThink := fl.Bool("no-think", false, "ignore brief.json and render the degraded page")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(stderr, "render: --dir is required")
		return 2
	}
	path, err := renderDir(*dir, *noThink)
	if err != nil {
		fmt.Fprintf(stderr, "render: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, path)
	return 0
}

func renderDir(dir string, noThink bool) (string, error) {
	var raw Raw
	if err := readJSON(filepath.Join(dir, "raw.json"), &raw); err != nil {
		return "", err
	}
	var b *Brief
	var thinkErr error
	if noThink {
		thinkErr = errors.New("think skipped (--no-think)")
	} else {
		b, thinkErr = loadBrief(dir, raw)
	}
	html, err := renderHTML(join(raw, b, thinkErr))
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "brief.html")
	return out, os.WriteFile(out, html, 0o644)
}

// loadBrief returns the validated brief, or nil and the reason the page must be degraded.
func loadBrief(dir string, raw Raw) (*Brief, error) {
	var b Brief
	err := readJSON(filepath.Join(dir, "brief.json"), &b)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("no brief.json (think has not run)")
	}
	if err != nil {
		return nil, err
	}
	if errs := Validate(b, raw); len(errs) > 0 {
		return nil, fmt.Errorf("brief.json failed validation (%d problems, first: %v)", len(errs), errs[0])
	}
	return &b, nil
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
