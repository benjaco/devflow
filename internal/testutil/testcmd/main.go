package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testcmd <emit|write|serve>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "emit":
		if len(os.Args) > 2 && os.Args[2] != "" {
			fmt.Fprintln(os.Stdout, os.Args[2])
		}
		if len(os.Args) > 3 && os.Args[3] != "" {
			fmt.Fprintln(os.Stderr, os.Args[3])
		}
	case "write":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: testcmd write <path> <contents>")
			os.Exit(2)
		}
		if err := os.MkdirAll(filepath.Dir(os.Args[2]), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(os.Args[2], []byte(os.Args[3]), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "serve":
		runServe()
	default:
		fmt.Fprintf(os.Stderr, "unknown testcmd command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runServe() {
	if delay := os.Getenv("TESTCMD_READY_DELAY_MS"); delay != "" {
		ms, err := strconv.Atoi(delay)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	if readyPath := os.Getenv("TESTCMD_READY_FILE"); readyPath != "" {
		if err := os.MkdirAll(filepath.Dir(readyPath), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(readyPath, []byte("ready\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	interval := 100 * time.Millisecond
	if raw := os.Getenv("TESTCMD_INTERVAL_MS"); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		interval = time.Duration(ms) * time.Millisecond
	}
	line := os.Getenv("TESTCMD_STDOUT_LINE")
	for {
		if line != "" {
			fmt.Println(os.ExpandEnv(line))
		}
		time.Sleep(interval)
	}
}
