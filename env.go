package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Env is everything a collector may touch (doc 03 §0.2). No globals: tests build an Env with fakes.
type Env struct {
	Config  Config
	Now     time.Time      // injected clock; only main.go calls time.Now()
	Loc     *time.Location // your zone
	Run     func(ctx context.Context, name string, args ...string) ([]byte, error)
	DataDir string // run folders live here: <DataDir>/<YYYY-MM-DD>/
	RunDir  string // this run's folder, set by collect; connector transcripts are saved here ("" = don't save)
}

// Config is ~/.config/brief/config.json: non-secret settings only. Every field has a default,
// so a missing file means "defaults", not an error.
type Config struct {
	Timezone string   `json:"timezone,omitempty"` // IANA name, e.g. "Europe/London"; default: the system zone
	DataDir  string   `json:"data_dir,omitempty"` // default ~/.local/share/brief
	Disabled []string `json:"disabled,omitempty"` // source names to skip

	// think stage
	Model           string   `json:"model,omitempty"`             // claude model alias; default "sonnet"
	Interests       []string `json:"interests,omitempty"`         // for reading picks
	ClaudeConfigDir string   `json:"claude_config_dir,omitempty"` // which Claude account think uses; default: inherited
}

func loadConfig(path string) (Config, error) {
	var c Config
	err := readJSON(path, &c)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	return c, err
}

// newEnv builds the real Env. now is passed in so this file never reads the clock.
func newEnv(now time.Time) (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, err
	}
	cfg, err := loadConfig(filepath.Join(home, ".config", "brief", "config.json"))
	if err != nil {
		return Env{}, fmt.Errorf("config: %w", err)
	}
	if cfg.Timezone == "" {
		cfg.Timezone = systemZone()
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return Env{}, fmt.Errorf("config: timezone %q: %w", cfg.Timezone, err)
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(home, ".local", "share", "brief")
	}
	return Env{Config: cfg, Now: now.In(loc), Loc: loc, Run: execRun, DataDir: cfg.DataDir}, nil
}

// systemZone returns the IANA name of the system zone. time.Local.String() is just "Local",
// which can't be written to raw.json and loaded back, so read it from $TZ or /etc/localtime.
func systemZone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
			return name
		}
	}
	return "UTC"
}

// execRun runs a program and returns its stdout. On failure the error carries the first line of
// stderr, which is what the user needs to fix it. Never pass secrets as args (they show in `ps`).
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		if msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
		return out, err
	}
	return out, nil
}

func (env Env) runDir(d civilDate) string { return filepath.Join(env.DataDir, d.String()) }

// parseDateFlag accepts "today", "yesterday", or YYYY-MM-DD, relative to env.Now in env.Loc.
func (env Env) parseDateFlag(s string) (civilDate, error) {
	switch s {
	case "", "today":
		return dateOf(env.Now.In(env.Loc)), nil
	case "yesterday":
		return dateOf(env.Now.In(env.Loc).AddDate(0, 0, -1)), nil
	}
	return parseCivilDate(s)
}

func dateOf(t time.Time) civilDate { return civilDate{t.Year(), t.Month(), t.Day()} }

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
