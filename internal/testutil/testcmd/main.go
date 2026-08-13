package main

import (
	"bufio"
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
	case "npm":
		runFakeNPM()
		return
	case "pg_dump":
		runFakePGDump()
		return
	case "psql":
		runFakePSQL()
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: testcmd <emit|write|write-after|write-if-missing|serve>")
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
	case "write-after":
		runWriteAfter()
	case "write-if-missing":
		runWriteIfMissing()
	case "serve":
		runServe()
	default:
		fmt.Fprintf(os.Stderr, "unknown testcmd command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runWriteAfter() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: testcmd write-after <counter-path> <attempt> <output-path> <contents>")
		os.Exit(2)
	}
	wantAttempt, err := strconv.Atoi(os.Args[3])
	if err != nil || wantAttempt < 1 {
		fmt.Fprintln(os.Stderr, "write-after attempt must be a positive integer")
		os.Exit(2)
	}
	attempt := 0
	if data, readErr := os.ReadFile(os.Args[2]); readErr == nil {
		attempt, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	} else if !os.IsNotExist(readErr) {
		fmt.Fprintln(os.Stderr, readErr)
		os.Exit(1)
	}
	attempt++
	if err := os.MkdirAll(filepath.Dir(os.Args[2]), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], []byte(strconv.Itoa(attempt)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if attempt < wantAttempt {
		fmt.Printf("successful attempt %d without output\n", attempt)
		return
	}
	if err := os.MkdirAll(filepath.Dir(os.Args[4]), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[4], []byte(os.Args[5]), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote output on attempt %d\n", attempt)
}

func runWriteIfMissing() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: testcmd write-if-missing <missing-path-1> <missing-path-2> <output-path> <contents>")
		os.Exit(2)
	}
	for _, path := range os.Args[2:4] {
		if _, err := os.Lstat(path); err == nil {
			fmt.Fprintf(os.Stderr, "expected %s to be missing\n", path)
			os.Exit(1)
		} else if !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := os.MkdirAll(filepath.Dir(os.Args[4]), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[4], []byte(os.Args[5]), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runFakePGDump() {
	if os.Getenv("DEVFLOW_FAKE_PG_DUMP_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "version mismatch")
		os.Exit(42)
	}
	dumpPath := ""
	remoteURL := ""
	for index := 1; index < len(os.Args); index++ {
		if os.Args[index] == "-f" && index+1 < len(os.Args) {
			dumpPath = os.Args[index+1]
			index++
			continue
		}
		if !strings.HasPrefix(os.Args[index], "-") {
			remoteURL = os.Args[index]
		}
	}
	if dumpPath == "" || remoteURL == "" {
		fmt.Fprintln(os.Stderr, "fake pg_dump requires -f and a remote URL")
		os.Exit(2)
	}
	if err := os.WriteFile(dumpPath, []byte("dump:"+remoteURL), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runFakePSQL() {
	if marker := os.Getenv("DEVFLOW_FAKE_PSQL_RECORD"); marker != "" {
		if err := os.WriteFile(marker, []byte("ran\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	dumpPath := ""
	for index := 1; index < len(os.Args); index++ {
		if os.Args[index] == "-f" && index+1 < len(os.Args) {
			dumpPath = os.Args[index+1]
			break
		}
	}
	output := os.Getenv("OUT_FILE")
	if dumpPath == "" || output == "" {
		fmt.Fprintln(os.Stderr, "fake psql requires -f and OUT_FILE")
		os.Exit(2)
	}
	data, err := os.ReadFile(dumpPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(output, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(output+".url", []byte(os.Getenv("DATABASE_URL")), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
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

func runFakeNPM() {
	appendNPMRecord("npm " + strings.Join(os.Args[1:], " "))
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "install" {
		if err := os.MkdirAll(".devflow/fake-node", 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(".devflow/fake-node/install.stamp", []byte("installed\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("fake npm install ok")
		return
	}
	if len(args) >= 4 && args[0] == "run" && args[1] == "payload" && args[2] == "--" {
		runFakePayload(args[3:])
		return
	}
	if len(args) >= 1 && args[0] == "payload" {
		runFakePayload(args[1:])
		return
	}
	fmt.Fprintf(os.Stderr, "fake npm unsupported args: %s\n", strings.Join(args, " "))
	os.Exit(2)
}

func runFakePayload(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fake payload requires command")
		os.Exit(2)
	}
	switch args[0] {
	case "migrate":
		count := countMigrationFiles(filepath.Join("src", "migrations"))
		if err := os.MkdirAll(filepath.Join(".devflow", "payload-test"), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(".devflow", "payload-test", "applied.txt"), []byte(fmt.Sprintf("%d migrations\n", count)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("Applied %d PayloadCMS migrations\n", count)
	case "migrate:create":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			fmt.Fprintln(os.Stderr, "migration name is required")
			os.Exit(2)
		}
		if fakePayloadSkipsMigrationCreate() {
			fmt.Println("Payload migrate:create exited successfully without writing a migration")
			return
		}
		if fakePayloadNeedsDestructiveConfirmation() && !hasArg(args[2:], "--force-accept-warning") {
			reader := bufio.NewReader(os.Stdin)
			fmt.Print("DATA LOSS WARNING: dropping a field may delete data. Accept warnings and create migration? [y/N]: ")
			answer, err := reader.ReadString('\n')
			if err != nil {
				fmt.Fprintf(os.Stderr, "read payload confirm: %v\n", err)
				os.Exit(1)
			}
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer != "y" && answer != "yes" {
				fmt.Fprintln(os.Stderr, "Payload migration cancelled")
				os.Exit(2)
			}
		}
		name := strings.TrimSpace(args[1])
		stamp := os.Getenv("DEVFLOW_FAKE_MIGRATION_TIMESTAMP")
		if stamp == "" {
			stamp = time.Now().UTC().Format("20060102150405")
		}
		dir := filepath.Join("src", "migrations")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		file := filepath.Join(dir, stamp+"_"+slugName(name)+".ts")
		content := fmt.Sprintf("export async function up() { /* %s */ }\nexport async function down() {}\n", name)
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("Created migration %s\n", file)
	default:
		fmt.Fprintf(os.Stderr, "fake payload unsupported command: %s\n", strings.Join(args, " "))
		os.Exit(2)
	}
}

func fakePayloadSkipsMigrationCreate() bool {
	skip, err := strconv.Atoi(os.Getenv("DEVFLOW_FAKE_PAYLOAD_SKIP_CREATE_SUCCESSES"))
	if err != nil || skip < 1 {
		return false
	}
	recordPath := os.Getenv("DEVFLOW_FAKE_NPM_RECORD")
	if recordPath == "" {
		return false
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return false
	}
	return strings.Count(string(data), "migrate:create") <= skip
}

func fakePayloadNeedsDestructiveConfirmation() bool {
	switch os.Getenv("DEVFLOW_FAKE_PAYLOAD_REQUIRE_CONFIRM") {
	case "1", "true":
		return true
	case "deleted-field":
		data, err := os.ReadFile(filepath.Join("src", "collections", "Posts.ts"))
		return err == nil && !strings.Contains(string(data), "legacy")
	default:
		return false
	}
}

func countMigrationFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".cjs") {
			count++
		}
	}
	return count
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func slugName(value string) string {
	value = strings.ToLower(value)
	var out strings.Builder
	lastUnderscore := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			out.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			out.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(out.String(), "_")
}

func appendNPMRecord(line string) {
	path := os.Getenv("DEVFLOW_FAKE_NPM_RECORD")
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
