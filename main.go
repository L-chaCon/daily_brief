package main

import (
	"fmt"
	"io"
	"os"
)

const usage = "usage: collect, think, render, run, doctor, connect, open, help"

func main() {
	args := os.Args
	code := run(args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}

	command := args[0]
	switch command {
	case "-h", "help", "--help":
		_, _ = fmt.Fprintln(stdout, usage)
	case "collect", "think", "render", "run", "doctor", "connect", "open":
		_, _ = fmt.Fprintf(stderr, "%s: not implemented\n", command)
		return 2
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", command)
		return 2
	}
	return 0
}
