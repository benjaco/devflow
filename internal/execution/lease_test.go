package execution

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/internal/lock"
)

func TestLeaseRejectsSameWorktreeAndPreservesOwner(t *testing.T) {
	worktree := t.TempDir()
	first, err := Acquire(worktree, Owner{Target: "dev", Mode: "watch", Kind: "daemon"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	before, err := ReadOwner(worktree)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(worktree, Owner{Target: "test", Mode: "ci"})
	if second != nil {
		_ = second.Release()
		t.Fatal("second owner admitted")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Code() != "resource_conflict" {
		t.Fatalf("want resource_conflict, got %v", err)
	}
	if conflict.RecoveryRequired || conflict.Owner == nil || conflict.Owner.Target != "dev" || conflict.Owner.PID != os.Getpid() {
		t.Fatalf("wrong active owner: %+v", conflict)
	}
	after, err := ReadOwner(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if *before != *after {
		t.Fatalf("conflict changed owner: before=%+v after=%+v", before, after)
	}
	other, err := Acquire(t.TempDir(), Owner{Target: "test", Mode: "ci"})
	if err != nil {
		t.Fatalf("independent worktree blocked: %v", err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if owner, err := ReadOwner(worktree); err != nil || owner != nil {
		t.Fatalf("released marker remains: owner=%+v err=%v", owner, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err = Acquire(worktree, Owner{Target: "test", Mode: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseUsesCanonicalWorktree(t *testing.T) {
	worktree := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(worktree, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	first, err := Acquire(worktree, Owner{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if !first.ValidFor(alias) {
		t.Fatal("canonical alias did not identify the owner")
	}
	second, err := Acquire(alias, Owner{Target: "test"})
	if second != nil {
		_ = second.Release()
		t.Fatal("alias bypassed ownership")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestLeaseTreatsRelativeAndAbsoluteWorktreeAsSameOwner(t *testing.T) {
	worktree := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, worktree)
	if err != nil {
		t.Skipf("worktree is on a different volume: %v", err)
	}
	lease, err := Acquire(relative, Owner{Target: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if !filepath.IsAbs(lease.Owner().Worktree) {
		t.Fatalf("owner stored noncanonical worktree %q", lease.Owner().Worktree)
	}
	if !lease.ValidFor(worktree) {
		t.Fatal("absolute spelling rejected caller's existing lease")
	}
}

func TestLeaseRecoveryHoldsWorktreeReservation(t *testing.T) {
	worktree := t.TempDir()
	first, err := Acquire(worktree, Owner{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Abandon(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Acquire(worktree, Owner{Target: "cleanup"}, WithRecovery(func(previous Owner) error {
		intruder, err := Acquire(worktree, Owner{Target: "test"})
		if intruder != nil {
			_ = intruder.Release()
			t.Fatal("competing execution entered during recovery")
		}
		var conflict *ConflictError
		if !errors.As(err, &conflict) || conflict.RecoveryRequired {
			t.Fatalf("recovery did not retain active reservation: %v", err)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestRequireRecoveryRetainsReservationUntilCallerFinishes(t *testing.T) {
	worktree := t.TempDir()
	lease, err := Acquire(worktree, Owner{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	lease.RequireRecovery()
	if lease.ValidFor(worktree) {
		t.Fatal("resource recovery requirement still grants borrowed execution")
	}
	reconciled := false
	next, err := Acquire(worktree, Owner{Target: "cleanup"}, WithRecovery(func(Owner) error {
		reconciled = true
		return nil
	}))
	if next != nil {
		_ = next.Release()
		t.Fatal("recovery entered while old owner still finalizes status and environment")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.RecoveryRequired || reconciled {
		t.Fatalf("incomplete owner lost active reservation: reconciled=%t err=%v", reconciled, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	next, err = Acquire(worktree, Owner{Target: "next"})
	if next != nil {
		_ = next.Release()
		t.Fatal("finalizing owner removed recovery marker")
	}
	if !errors.As(err, &conflict) || !conflict.RecoveryRequired {
		t.Fatalf("finished owner did not require recovery: %v", err)
	}
}

func TestAbandonRetainsRecoveryMarkerAndInvalidatesContext(t *testing.T) {
	worktree := t.TempDir()
	first, err := Acquire(worktree, Owner{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithLease(context.Background(), first)
	if FromContext(ctx) != first || !first.ValidFor(worktree) {
		t.Fatal("live lease missing from context")
	}
	if first.ValidFor(t.TempDir()) {
		t.Fatal("lease accepted another worktree")
	}
	if err := first.Abandon(); err != nil {
		t.Fatal(err)
	}
	if first.ValidFor(worktree) {
		t.Fatal("abandoned lease still grants execution")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	owner, err := ReadOwner(worktree)
	if err != nil || owner == nil {
		t.Fatalf("abandoned marker removed: %+v %v", owner, err)
	}
	var conflict *ConflictError
	_, err = Acquire(worktree, Owner{Target: "test"})
	if !errors.As(err, &conflict) || !conflict.RecoveryRequired {
		t.Fatalf("want recovery conflict, got %v", err)
	}
	recoveryErr := errors.New("service still owns build outputs")
	_, err = Acquire(worktree, Owner{Target: "test"}, WithRecovery(func(previous Owner) error {
		if previous.Token != owner.Token {
			t.Fatal("recovery lost previous ownership")
		}
		return recoveryErr
	}))
	if !errors.Is(err, recoveryErr) {
		t.Fatalf("lost recovery failure: %v", err)
	}
	retained, err := ReadOwner(worktree)
	if err != nil || retained == nil || retained.Token != owner.Token {
		t.Fatalf("failed recovery replaced owner: %+v %v", retained, err)
	}
	second, err := Acquire(worktree, Owner{Target: "test"}, WithRecovery(func(previous Owner) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if second.Owner().Token == owner.Token {
		t.Fatal("new admission reused abandoned token")
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseOwnerMetadataIsPrivateAndCorruptionIsPreserved(t *testing.T) {
	worktree := t.TempDir()
	lease, err := Acquire(worktree, Owner{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(worktree, ".devflow", "execution-owner.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("owner metadata mode = %o", info.Mode().Perm())
	}
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	const broken = "{broken state"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if next, err := Acquire(worktree, Owner{Target: "test"}); err == nil {
		_ = next.Release()
		t.Fatal("corrupt ownership silently replaced")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != broken {
		t.Fatalf("corrupt ownership not preserved: %q %v", data, err)
	}
}

func TestLeaseRejectsOtherProcessAndRequiresRecoveryAfterOwnerExit(t *testing.T) {
	worktree := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestExecutionLeaseHelper$")
	cmd.Env = append(os.Environ(), "DEVFLOW_TEST_LEASE_WORKTREE="+worktree)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("child not ready: %q %v", scanner.Text(), scanner.Err())
	}
	next, err := Acquire(worktree, Owner{Target: "test", Mode: "ci"})
	if next != nil {
		_ = next.Release()
		t.Fatal("other-process owner bypassed")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Owner == nil || conflict.Owner.PID != cmd.Process.Pid || conflict.RecoveryRequired {
		t.Fatalf("wrong cross-process conflict: %+v err=%v", conflict, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
	next, err = Acquire(worktree, Owner{Target: "test", Mode: "ci"})
	if next != nil {
		_ = next.Release()
		t.Fatal("owner death treated as completed resource cleanup")
	}
	if !errors.As(err, &conflict) || !conflict.RecoveryRequired {
		t.Fatalf("want abandoned owner conflict, got %v", err)
	}
	next, err = Acquire(worktree, Owner{Target: "test", Mode: "ci"}, WithRecovery(func(previous Owner) error {
		if previous.PID != cmd.Process.Pid || previous.Target != "dev" {
			t.Fatalf("wrong recovery owner: %+v", previous)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionLeaseHelper(t *testing.T) {
	worktree := os.Getenv("DEVFLOW_TEST_LEASE_WORKTREE")
	if worktree == "" {
		return
	}
	_, err := Acquire(worktree, Owner{Target: "dev", Mode: "watch", Kind: "daemon"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	// Deliberately exit without Release: the OS releases its lock, but resources
	// started by an execution need not exit with the owner process.
	os.Exit(0)
}

func TestReadOwnerDoesNotCreateState(t *testing.T) {
	worktree := t.TempDir()
	if owner, err := ReadOwner(worktree); owner != nil || err != nil {
		t.Fatalf("unexpected owner: %+v %v", owner, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".devflow")); !os.IsNotExist(err) {
		t.Fatalf("read created state: %v", err)
	}
	if FromContext(context.Background()) != nil {
		t.Fatal("unexpected context lease")
	}
	var empty *Lease
	if empty.ValidFor(worktree) {
		t.Fatal("nil lease grants execution")
	}
	if err := empty.Release(); err != nil {
		t.Fatal(err)
	}
	if err := empty.Abandon(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace((&ConflictError{}).Error()) == "" {
		t.Fatal("empty conflict message")
	}
}

func TestConflictIdentifiesWorktreeBeforeOwnerMetadataPublication(t *testing.T) {
	worktree := t.TempDir()
	held, err := lock.TryAcquire(filepath.Join(worktree, ".devflow", "execution.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	next, err := Acquire(worktree, Owner{Target: "contender"})
	if next != nil {
		_ = next.Release()
		t.Fatal("contender entered before owner metadata publication")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	want, err := canonicalWorktree(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Owner != nil || conflict.Worktree != want {
		t.Fatalf("conflict lost worktree before owner publication: %+v", conflict)
	}
}
