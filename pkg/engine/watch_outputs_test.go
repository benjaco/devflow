package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/benjaco/devflow/pkg/project"
)

func TestWatchOutputEvidenceRetainsEditAfterProducerCompletion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	writeOutputEvidenceFile(t, path, "original source")
	finish := beginWatchOutputs(context.Background(), root, project.Outputs{Files: []string{"source.txt"}})
	writeOutputEvidenceFile(t, path, "FORMATTED SOURCE")
	evidence := []watchOutputEvidence{finish()}
	if got := filterProducedWatchOutputs(root, []string{"source.txt"}, evidence); len(got) != 0 {
		t.Fatalf("producer's unchanged output was not suppressed: %v", got)
	}
	writeOutputEvidenceFile(t, path, "a newer external edit")
	if got := filterProducedWatchOutputs(root, []string{"source.txt"}, evidence); !reflect.DeepEqual(got, []string{"source.txt"}) {
		t.Fatalf("producer evidence hid the later source edit: %v", got)
	}
}

func TestWatchOutputEvidenceRetainsPreexistingAncestors(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	finish := beginWatchOutputs(context.Background(), root, project.Outputs{Files: []string{"shared/out.txt"}})
	writeOutputEvidenceFile(t, filepath.Join(root, "shared", "out.txt"), "generated")
	evidence := []watchOutputEvidence{finish()}
	if got := filterProducedWatchOutputs(root, []string{"shared", "shared/out.txt", "shared/source.txt"}, evidence); !reflect.DeepEqual(got, []string{"shared", "shared/source.txt"}) {
		t.Fatalf("output suppressed a preexisting parent or sibling: %v", got)
	}
}

func TestWatchOutputEvidenceTracksCreatedAncestors(t *testing.T) {
	root := t.TempDir()
	finish := beginWatchOutputs(context.Background(), root, project.Outputs{Paths: []string{"new/deep/out.txt"}})
	writeOutputEvidenceFile(t, filepath.Join(root, "new", "deep", "out.txt"), "generated")
	evidence := []watchOutputEvidence{finish()}
	files := []string{"new", "new/deep", "new/deep/out.txt"}
	if got := filterProducedWatchOutputs(root, files, evidence); len(got) != 0 {
		t.Fatalf("producer-created parents caused a redundant rerun: %v", got)
	}
	if err := os.RemoveAll(filepath.Join(root, "new")); err != nil {
		t.Fatal(err)
	}
	if got := filterProducedWatchOutputs(root, files, evidence); !reflect.DeepEqual(got, files) {
		t.Fatalf("later deletion of output parents was suppressed: %v", got)
	}
}

func TestWatchOutputEvidenceDistinguishesDirectoryChangesAfterCompletion(t *testing.T) {
	for _, paths := range []bool{false, true} {
		name := "dirs"
		outputs := project.Outputs{Dirs: []string{"out"}}
		if paths {
			name = "paths"
			outputs = project.Outputs{Paths: []string{"out"}}
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeOutputEvidenceFile(t, filepath.Join(root, "out", "removed-by-producer"), "old")
			finish := beginWatchOutputs(context.Background(), root, outputs)
			if err := os.Remove(filepath.Join(root, "out", "removed-by-producer")); err != nil {
				t.Fatal(err)
			}
			writeOutputEvidenceFile(t, filepath.Join(root, "out", "kept"), "generated")
			writeOutputEvidenceFile(t, filepath.Join(root, "out", "removed-later"), "generated")
			evidence := []watchOutputEvidence{finish()}
			if err := os.Remove(filepath.Join(root, "out", "removed-later")); err != nil {
				t.Fatal(err)
			}
			writeOutputEvidenceFile(t, filepath.Join(root, "out", "added-later"), "user edit")
			got := filterProducedWatchOutputs(root, []string{"out/removed-by-producer", "out/kept", "out/removed-later", "out/added-later"}, evidence)
			want := []string{"out/added-later", "out/removed-later"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("directory evidence hid changes made after completion: got %v, want %v", got, want)
			}
		})
	}
}

func TestWatchOutputEvidenceIncompleteCapturePreventsOlderFallback(t *testing.T) {
	root := t.TempDir()
	writeOutputEvidenceFile(t, filepath.Join(root, "source.txt"), "original")
	outputs := project.Outputs{Files: []string{"source.txt"}}
	first := beginWatchOutputs(context.Background(), root, outputs)()
	ctx, cancel := context.WithCancel(context.Background())
	finish := beginWatchOutputs(ctx, root, outputs)
	cancel()
	second := finish()
	got := filterProducedWatchOutputs(root, []string{"source.txt"}, []watchOutputEvidence{first, second})
	if !reflect.DeepEqual(got, []string{"source.txt"}) {
		t.Fatalf("incomplete latest snapshot fell back to old producer evidence: %v", got)
	}
}

func TestWatchOutputEvidenceDoesNotFollowDirectorySymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeOutputEvidenceFile(t, filepath.Join(outside, "source.txt"), "outside input")
	if err := os.Mkdir(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "out", "link")); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	evidence := []watchOutputEvidence{beginWatchOutputs(context.Background(), root, project.Outputs{Dirs: []string{"out"}})()}
	if err := os.Remove(filepath.Join(outside, "source.txt")); err != nil {
		t.Fatal(err)
	}
	got := filterProducedWatchOutputs(root, []string{"out/link/source.txt"}, evidence)
	if !reflect.DeepEqual(got, []string{"out/link/source.txt"}) {
		t.Fatalf("output snapshot claimed an unobserved symlink descendant: %v", got)
	}
}

func writeOutputEvidenceFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWatchOutputEvidenceRetainsAncestorModeChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not provide Unix directory permission modes")
	}
	root := t.TempDir()
	parent := filepath.Join(root, "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	finish := beginWatchOutputs(context.Background(), root, project.Outputs{Files: []string{"shared/out.txt"}})
	writeOutputEvidenceFile(t, filepath.Join(parent, "out.txt"), "generated")
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	got := filterProducedWatchOutputs(root, []string{"shared", "shared/out.txt"}, []watchOutputEvidence{finish()})
	if !reflect.DeepEqual(got, []string{"shared"}) {
		t.Fatalf("producer output hid a preexisting ancestor permission change: %v", got)
	}
}

func TestWatchOutputEvidenceTracksDeletedDirectoryOutputs(t *testing.T) {
	for _, paths := range []bool{false, true} {
		name := "dirs"
		outputs := project.Outputs{Dirs: []string{"out"}}
		if paths {
			name = "paths"
			outputs = project.Outputs{Paths: []string{"out"}}
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeOutputEvidenceFile(t, filepath.Join(root, "out", "child"), "old")
			finish := beginWatchOutputs(context.Background(), root, outputs)
			if err := os.RemoveAll(filepath.Join(root, "out")); err != nil {
				t.Fatal(err)
			}
			evidence := []watchOutputEvidence{finish()}
			files := []string{"out", "out/child"}
			if got := filterProducedWatchOutputs(root, files, evidence); len(got) != 0 {
				t.Fatalf("producer directory deletion was not recognized: %v", got)
			}
			writeOutputEvidenceFile(t, filepath.Join(root, "out", "child"), "later creation")
			if got := filterProducedWatchOutputs(root, files, evidence); !reflect.DeepEqual(got, files) {
				t.Fatalf("later recreation of a deleted output was hidden: %v", got)
			}
		})
	}
}

func TestWatchOutputEvidenceScanFailurePreventsOlderFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows chmod does not make a directory unreadable")
	}
	root := t.TempDir()
	path := filepath.Join(root, "out")
	writeOutputEvidenceFile(t, filepath.Join(path, "child"), "generated")
	outputs := project.Outputs{Dirs: []string{"out"}}
	first := beginWatchOutputs(context.Background(), root, outputs)()
	finish := beginWatchOutputs(context.Background(), root, outputs)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o700)
	if _, err := os.ReadDir(path); err == nil {
		t.Skip("current user can read directories regardless of permission bits")
	}
	second := finish()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	got := filterProducedWatchOutputs(root, []string{"out/child"}, []watchOutputEvidence{first, second})
	if !reflect.DeepEqual(got, []string{"out/child"}) {
		t.Fatalf("failed directory snapshot reused obsolete evidence: %v", got)
	}
}

func TestWatchOutputEvidenceRetainsSymlinkTargetEdits(t *testing.T) {
	for _, directoryScope := range []bool{false, true} {
		name := "file"
		outputs := project.Outputs{Files: []string{"out/source.txt"}}
		if directoryScope {
			name = "directory"
			outputs = project.Outputs{Dirs: []string{"out"}}
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(t.TempDir(), "source.txt")
			writeOutputEvidenceFile(t, target, "initial target")
			if err := os.Mkdir(filepath.Join(root, "out"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "out", "source.txt")); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			evidence := []watchOutputEvidence{beginWatchOutputs(context.Background(), root, outputs)()}
			writeOutputEvidenceFile(t, target, "newer external target contents")
			got := filterProducedWatchOutputs(root, []string{"out/source.txt"}, evidence)
			if !reflect.DeepEqual(got, []string{"out/source.txt"}) {
				t.Fatalf("unchanged symlink metadata hid a target edit: %v", got)
			}
		})
	}
}
