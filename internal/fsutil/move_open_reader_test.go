package fsutil

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMoveWithOpenReaderRetriesAfterReaderCloses(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "worktree with spaces")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(root, "existing output")
			contentPath := source
			if kind == "directory" {
				if err := os.Mkdir(source, 0o755); err != nil {
					t.Fatal(err)
				}
				contentPath = filepath.Join(source, "client.txt")
			}
			const content = "reader and moved output must retain these bytes"
			if err := os.WriteFile(contentPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			// Establish the original single-rename behavior with a real reader.
			// The control also runs where an open reader permits publication.
			reader, err := os.Open(contentPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			controlDestination := filepath.Join(root, "single rename control")
			controlErr := os.Rename(source, controlDestination)
			if data, err := io.ReadAll(reader); err != nil || string(data) != content {
				t.Fatalf("single rename changed reader bytes: %q, %v", data, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if controlErr == nil {
				if err := os.Rename(controlDestination, source); err != nil {
					t.Fatal(err)
				}
			} else {
				var native *os.LinkError
				if !errors.As(controlErr, &native) || native.Old != source || native.New != controlDestination {
					t.Fatalf("single rename failed outside the observed operation: %v", controlErr)
				}
				if data, err := os.ReadFile(contentPath); err != nil || string(data) != content {
					t.Fatalf("refused single rename changed source: %q, %v", data, err)
				}
				// The same rename must work after closing our reader. This rules
				// out treating an unrelated permanent fixture error as the repro.
				if err := os.Rename(source, controlDestination); err != nil {
					t.Fatalf("unlocked control failed: %v", err)
				}
				if err := os.Rename(controlDestination, source); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("single-rename control with open %s reader: %v", kind, controlErr)

			reader, err = os.Open(contentPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			destination := filepath.Join(root, "transaction backup")
			attempts := 0
			var firstFailure error
			var readerContent []byte
			moveErr := movePathWritable(context.Background(), source, destination, func(oldPath, newPath string) error {
				attempts++
				renameErr := os.Rename(oldPath, newPath)
				if renameErr != nil && firstFailure == nil {
					firstFailure = renameErr
					var readErr error
					readerContent, readErr = io.ReadAll(reader)
					if readErr != nil {
						t.Fatal(readErr)
					}
					// Release only after the OS refused the actual rename. A
					// timed release could miss the conflict on a loaded runner.
					if err := reader.Close(); err != nil {
						t.Fatal(err)
					}
				}
				return renameErr
			}, transientRenameError)
			if moveErr != nil {
				t.Fatalf("move did not recover after its reader closed: attempts=%d firstFailure=%v err=%v", attempts, firstFailure, moveErr)
			}
			if firstFailure == nil {
				readerContent, err = io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if string(readerContent) != content {
				t.Fatalf("move changed open-reader bytes: %q", readerContent)
			}
			if controlErr != nil && (firstFailure == nil || attempts < 2) {
				t.Fatalf("open-reader refusal was not retried: attempts=%d firstFailure=%v control=%v", attempts, firstFailure, controlErr)
			}
			if firstFailure != nil {
				var native *os.LinkError
				if !errors.As(firstFailure, &native) || native.Old != source || native.New != destination {
					t.Fatalf("retry did not follow the actual rename refusal: %v", firstFailure)
				}
			}
			movedContent := destination
			if kind == "directory" {
				movedContent = filepath.Join(destination, "client.txt")
			}
			if data, err := os.ReadFile(movedContent); err != nil || string(data) != content {
				t.Fatalf("moved output bytes = %q, %v", data, err)
			}
			if _, err := os.Stat(source); !os.IsNotExist(err) {
				t.Fatalf("source remains after successful move: %v", err)
			}
			t.Logf("move completed: attempts=%d firstFailure=%v", attempts, firstFailure)
		})
	}
}
