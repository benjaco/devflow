package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "go":
		runFakeGo()
		return
	case "dlv":
		runFakeDlv()
		return
	}
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

func runFakeGo() {
	if len(os.Args) == 0 || len(os.Args) < 2 || os.Args[1] != "build" {
		fmt.Fprintf(os.Stderr, "fake go only supports build, got %q\n", strings.Join(os.Args[1:], " "))
		os.Exit(2)
	}
	out := ""
	for i := 2; i < len(os.Args); i++ {
		if os.Args[i] == "-o" && i+1 < len(os.Args) {
			out = os.Args[i+1]
			break
		}
	}
	if out == "" {
		fmt.Fprintln(os.Stderr, "fake go build requires -o")
		os.Exit(2)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, []byte("fake debug binary\n"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	appendRecord("fake-go " + strings.Join(os.Args[1:], " "))
	fmt.Println("fake go build ok")
}

func runFakeDlv() {
	listen := ""
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--listen=") {
			listen = strings.TrimPrefix(arg, "--listen=")
			break
		}
	}
	if listen == "" {
		fmt.Fprintln(os.Stderr, "fake dlv requires --listen")
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ln.Close()
	appendRecord(fmt.Sprintf("fake-dlv pid=%d listen=%s args=%s", os.Getpid(), listen, strings.Join(os.Args[1:], " ")))
	fmt.Printf("API server listening at: %s\n", listen)
	for {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
			continue
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func appendRecord(line string) {
	path := os.Getenv("DEVFLOW_FAKE_DEBUG_RECORD")
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintln(file, line)
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
