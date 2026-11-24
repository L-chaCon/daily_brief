package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
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
	case "collect", "open", "run":
		env, err := newEnv(time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
			return 1
		}
		switch cmd {
		case "collect":
			return cmdCollect(env, rest, stdout, stderr)
		case "open":
			return cmdOpen(env, rest, stdout, stderr)
		default:
			return cmdRun(env, rest, stdout, stderr)
		}
	case "think", "doctor", "connect":
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
	dir := fl.String("dir", "", "run folder containing raw.json (and brief.json); overrides --date")
	date := fl.String("date", "today", "today, yesterday, or YYYY-MM-DD")
	noThink := fl.Bool("no-think", false, "ignore brief.json and render the degraded page")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		env, err := newEnv(time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "render: %v\n", err)
			return 1
		}
		d, err := env.parseDateFlag(*date)
		if err != nil {
			fmt.Fprintf(stderr, "render: %v\n", err)
			return 2
		}
		*dir = env.runDir(d)
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

// dateFlag parses the common --date flag for commands that only need a date.
func dateFlag(env Env, name string, args []string, stderr io.Writer) (civilDate, bool) {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	date := fl.String("date", "today", "today, yesterday, or YYYY-MM-DD")
	if err := fl.Parse(args); err != nil {
		return civilDate{}, false
	}
	d, err := env.parseDateFlag(*date)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return civilDate{}, false
	}
	return d, true
}

// cmdCollect writes <run dir>/raw.json. Exit 0 even if sources failed: their status is in the file.
func cmdCollect(env Env, args []string, stdout, stderr io.Writer) int {
	d, ok := dateFlag(env, "collect", args, stderr)
	if !ok {
		return 2
	}
	path, err := collectTo(env, d, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "collect: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, path)
	return 0
}

func collectTo(env Env, d civilDate, stderr io.Writer) (string, error) {
	raw := collectAll(context.Background(), env, d, sources)
	for _, s := range raw.Sources {
		mark := "✓"
		if !s.OK {
			mark = "✗"
		}
		fmt.Fprintf(stderr, "%s %-10s %3d items %5dms %s\n", mark, s.Name, s.Count, s.TookMS, s.Error)
	}
	path := filepath.Join(env.runDir(d), "raw.json")
	return path, writeJSONFile(path, raw)
}

func cmdOpen(env Env, args []string, stdout, stderr io.Writer) int {
	d, ok := dateFlag(env, "open", args, stderr)
	if !ok {
		return 2
	}
	path := filepath.Join(env.runDir(d), "brief.html")
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(stderr, "open: no brief for %s. Run: brief run --date %s\n", d, d)
		return 1
	}
	if err := exec.Command("open", path).Run(); err != nil {
		fmt.Fprintf(stderr, "open: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, path)
	return 0
}

// cmdRun is the whole pipeline for one day: collect, (think), render, open.
func cmdRun(env Env, args []string, stdout, stderr io.Writer) int {
	d, ok := dateFlag(env, "run", args, stderr)
	if !ok {
		return 2
	}
	if _, err := collectTo(env, d, stderr); err != nil {
		fmt.Fprintf(stderr, "run: collect: %v\n", err)
		return 1
	}
	if _, err := renderDir(env.runDir(d), false); err != nil {
		fmt.Fprintf(stderr, "run: render: %v\n", err)
		return 1
	}
	return cmdOpen(env, []string{"--date", d.String()}, stdout, stderr)
}
