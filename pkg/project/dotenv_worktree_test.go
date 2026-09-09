package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDotEnvWorktreeFallback(t *testing.T) {
	for _, tc := range []struct {
		name, local string
		present     bool
		want        map[string]string
		wantError   bool
	}{
		{name: "missing_local_file_uses_main_checkout", want: map[string]string{"SOURCE": "main", "MAIN_ONLY": "yes"}},
		{name: "existing_local_file_does_not_merge_main", present: true, local: "SOURCE=linked\n", want: map[string]string{"SOURCE": "linked"}},
		{name: "empty_local_file_suppresses_fallback", present: true, want: map[string]string{}},
		{name: "malformed_local_file_returns_error", present: true, local: "invalid dotenv line\n", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotEnvGitEnvironment(t)
			mainBase := t.TempDir()
			main := filepath.Join(mainBase, "main checkout")
			linked := filepath.Join(mainBase, "worktrees", "linked checkout")
			if err := os.Mkdir(main, 0o755); err != nil {
				t.Fatal(err)
			}
			dotenvGit(t, main, "init")
			dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
			dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
			mainFile := filepath.Join(main, ".env")
			// Write after worktree creation to reproduce an untracked main-checkout file.
			writeDotEnvFile(t, mainFile, "SOURCE=main\nMAIN_ONLY=yes\n")
			path := filepath.Join(linked, ".env")
			if tc.present {
				writeDotEnvFile(t, path, tc.local)
			}
			env, err := LoadOptionalDotEnvInWorktree(linked, ".env")
			if data, readErr := os.ReadFile(mainFile); readErr != nil || string(data) != "SOURCE=main\nMAIN_ONLY=yes\n" {
				t.Fatalf("main dotenv changed: %q err=%v", data, readErr)
			}
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("expected local parse error, got env=%v err=%v", env, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(env, tc.want) {
				t.Fatalf("dotenv=%v err=%v, want %v", env, err, tc.want)
			}
			if !tc.present {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("fallback created a local dotenv file: %v", err)
				}
			}
		})
	}
}

func TestDotEnvWorktreeNestedProjectPreservesEnvLayering(t *testing.T) {
	isolateDotEnvGitEnvironment(t)
	mainBase := t.TempDir()
	main := filepath.Join(mainBase, "main checkout")
	linked := filepath.Join(mainBase, "worktrees", "linked checkout")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	dotenvGit(t, main, "init")
	dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
	writeDotEnvFile(t, filepath.Join(main, ".env"), "ROOT_ONLY=yes\n")
	if err := os.Mkdir(filepath.Join(main, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDotEnvFile(t, filepath.Join(main, "app", ".env"), "SOURCE=nested\nORDER=base\nSTATIC=dotenv\n")
	writeDotEnvFile(t, filepath.Join(main, "app", ".env.local"), "ORDER=override\n")
	selected := filepath.Join(linked, "app")
	if err := os.MkdirAll(selected, 0o755); err != nil {
		t.Fatal(err)
	}
	p := Define(func(_ context.Context, b *Builder) error {
		b.Name("dotenv-worktree-order").DotEnv(".env", ".env.local").Env("STATIC", "adapter")
		b.Target("verify", b.Task("check").NoCache())
		return nil
	})
	cfg, err := p.ConfigureInstance(context.Background(), selected)
	want := map[string]string{"SOURCE": "nested", "ORDER": "override", "STATIC": "adapter"}
	if err != nil || !reflect.DeepEqual(cfg.Env, want) {
		t.Fatalf("nested dotenv layers=%v err=%v, want %v", cfg.Env, err, want)
	}
}

func TestDotEnvWorktreeRelativeDeclarationUsesMatchingMainPath(t *testing.T) {
	isolateDotEnvGitEnvironment(t)
	mainBase := t.TempDir()
	main := filepath.Join(mainBase, "main checkout")
	linked := filepath.Join(mainBase, "worktrees", "linked checkout")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	dotenvGit(t, main, "init")
	dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
	if err := os.Mkdir(filepath.Join(main, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDotEnvFile(t, filepath.Join(main, "app", ".env"), "SOURCE=nested\n")
	env, err := LoadOptionalDotEnvInWorktree(linked, filepath.Join("app", ".env"))
	if err != nil || env["SOURCE"] != "nested" {
		t.Fatalf("relative declared path did not map into main checkout: %v %v", env, err)
	}
}

func TestDotEnvWorktreeFallbackBoundaries(t *testing.T) {
	t.Run("main_checkout_does_not_borrow_from_linked_checkout", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(linked, ".env"), "SOURCE=sibling\n")
		if env, err := LoadOptionalDotEnvInWorktree(main, ".env"); err != nil || len(env) != 0 {
			t.Fatalf("main checkout borrowed sibling dotenv: env=%v err=%v", env, err)
		}
	})
	t.Run("both_files_missing_returns_empty_environment", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		if env, err := LoadOptionalDotEnvInWorktree(linked, ".env"); err != nil || len(env) != 0 {
			t.Fatalf("missing optional dotenv: env=%v err=%v", env, err)
		}
	})
	t.Run("missing_absolute_path_does_not_fall_back", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(main, ".env"), "SOURCE=main\n")
		if env, err := LoadOptionalDotEnvInWorktree(linked, filepath.Join(linked, ".env")); err != nil || len(env) != 0 {
			t.Fatalf("absolute path borrowed main dotenv: env=%v err=%v", env, err)
		}
	})
	t.Run("path_outside_git_root_does_not_fall_back", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		// Different parent depths leave only the incorrect fallback location populated.
		writeDotEnvFile(t, filepath.Join(filepath.Dir(main), "outside.env"), "SOURCE=wrong-parent\n")
		if env, err := LoadOptionalDotEnvInWorktree(linked, filepath.Join("..", "outside.env")); err != nil || len(env) != 0 {
			t.Fatalf("outside-root path borrowed main dotenv: env=%v err=%v", env, err)
		}
	})
	t.Run("non_git_directory_returns_empty_environment", func(t *testing.T) {
		if env, err := LoadOptionalDotEnvInWorktree(t.TempDir(), ".env"); err != nil || len(env) != 0 {
			t.Fatalf("missing dotenv outside Git: env=%v err=%v", env, err)
		}
	})
}

func TestDotEnvWorktreeGitEnvironmentCannotRedirectLookup(t *testing.T) {
	isolateDotEnvGitEnvironment(t)
	mainBase := t.TempDir()
	main := filepath.Join(mainBase, "main checkout")
	linked := filepath.Join(mainBase, "worktrees", "linked checkout")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	dotenvGit(t, main, "init")
	dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
	otherBase := t.TempDir()
	other := filepath.Join(otherBase, "main checkout")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	dotenvGit(t, other, "init")
	dotenvGit(t, other, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	writeDotEnvFile(t, filepath.Join(main, ".env"), "SOURCE=main\n")
	writeDotEnvFile(t, filepath.Join(other, ".env"), "SOURCE=wrong-repository\n")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	env, err := LoadOptionalDotEnvInWorktree(linked, ".env")
	if err != nil || env["SOURCE"] != "main" {
		t.Fatalf("Git environment redirected dotenv discovery: %v %v", env, err)
	}
}

func TestDotEnvWorktreeWithoutGit(t *testing.T) {
	t.Run("missing_local_file_returns_empty_environment", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(main, ".env"), "SOURCE=main\n")
		t.Setenv("PATH", t.TempDir())
		if env, err := LoadOptionalDotEnvInWorktree(linked, ".env"); err != nil || len(env) != 0 {
			t.Fatalf("missing optional dotenv without Git: env=%v err=%v", env, err)
		}
	})
	t.Run("existing_local_file_still_loads", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(linked, ".env"), "SOURCE=local\n")
		t.Setenv("PATH", t.TempDir())
		env, err := LoadOptionalDotEnvInWorktree(linked, ".env")
		if err != nil || env["SOURCE"] != "local" {
			t.Fatalf("local dotenv required Git: %v %v", env, err)
		}
	})
}

func TestDotEnvWorktreeReadErrorsAreReported(t *testing.T) {
	t.Run("local_directory_does_not_fall_back", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(main, ".env"), "SOURCE=main\n")
		if err := os.Mkdir(filepath.Join(linked, ".env"), 0o755); err != nil {
			t.Fatal(err)
		}
		if env, err := LoadOptionalDotEnvInWorktree(linked, ".env"); err == nil {
			t.Fatalf("local read error hidden by fallback: %v", env)
		}
	})
	t.Run("malformed_main_file_returns_its_path", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		path := filepath.Join(main, ".env")
		writeDotEnvFile(t, path, "invalid dotenv line\n")
		if env, err := LoadOptionalDotEnvInWorktree(linked, ".env"); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("main parse error hidden: %v %v", env, err)
		}
	})
}

func TestDotEnvWorktreeDoesNotLoadGitMetadata(t *testing.T) {
	t.Run("separate_git_directory_is_not_a_checkout", func(t *testing.T) {
		metadata := filepath.Join(t.TempDir(), "git metadata")
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init", "--separate-git-dir", metadata)
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(metadata, ".env"), "SOURCE=git-metadata\n")
		if env, err := LoadOptionalDotEnvInWorktree(linked, ".env"); err != nil || len(env) != 0 {
			t.Fatalf("borrowed dotenv from Git metadata: env=%v err=%v", env, err)
		}
	})
	t.Run("bare_repository_is_not_a_checkout", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		metadata := filepath.Join(t.TempDir(), "bare.git")
		dotenvGit(t, main, "clone", "--bare", main, metadata)
		linked := filepath.Join(t.TempDir(), "bare-linked")
		dotenvGit(t, metadata, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(metadata, ".env"), "SOURCE=git-metadata\n")
		if env, err := LoadOptionalDotEnvInWorktree(linked, ".env"); err != nil || len(env) != 0 {
			t.Fatalf("borrowed dotenv from bare repository: env=%v err=%v", env, err)
		}
	})
}

func TestDotEnvWorktreePathsPreserveNewlinesAndTrailingSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support newlines in directory names")
	}
	isolateDotEnvGitEnvironment(t)
	mainBase := t.TempDir()
	main := filepath.Join(mainBase, "main checkout")
	linked := filepath.Join(mainBase, "worktrees", "linked checkout")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	dotenvGit(t, main, "init")
	dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
	unusualMain := main + "\ntrailing space "
	if err := os.Rename(main, unusualMain); err != nil {
		t.Fatal(err)
	}
	main = unusualMain
	dotenvGit(t, main, "worktree", "repair")
	moved := linked + "\ntrailing space "
	dotenvGit(t, main, "worktree", "move", linked, moved)
	writeDotEnvFile(t, filepath.Join(main, ".env"), "SOURCE=main\n")
	env, err := LoadOptionalDotEnvInWorktree(moved, ".env")
	if err != nil || env["SOURCE"] != "main" {
		t.Fatalf("dotenv through unusual worktree path %q: %v %v", moved, env, err)
	}
}

func TestDotEnvWorktreeSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated Windows privileges")
	}
	t.Run("worktree_alias_loads_main_dotenv", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(main, ".env"), "SOURCE=main\n")
		alias := filepath.Join(t.TempDir(), "linked-alias")
		if err := os.Symlink(linked, alias); err != nil {
			t.Fatal(err)
		}
		env, err := LoadOptionalDotEnvInWorktree(alias, ".env")
		if err != nil || env["SOURCE"] != "main" {
			t.Fatalf("dotenv through worktree symlink: %v %v", env, err)
		}
	})
	t.Run("broken_local_dotenv_symlink_does_not_fall_back", func(t *testing.T) {
		isolateDotEnvGitEnvironment(t)
		mainBase := t.TempDir()
		main := filepath.Join(mainBase, "main checkout")
		linked := filepath.Join(mainBase, "worktrees", "linked checkout")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		dotenvGit(t, main, "init")
		dotenvGit(t, main, "-c", "user.name=Devflow Test", "-c", "user.email=devflow@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
		dotenvGit(t, main, "worktree", "add", "--detach", linked, "HEAD")
		writeDotEnvFile(t, filepath.Join(main, ".env"), "SOURCE=main\n")
		if err := os.Symlink(filepath.Join(linked, "absent.env"), filepath.Join(linked, ".env")); err != nil {
			t.Fatal(err)
		}
		if env, err := LoadOptionalDotEnvInWorktree(linked, ".env"); err == nil {
			t.Fatalf("broken explicit symlink silently borrowed main dotenv: %v", env)
		}
	})
}

func isolateDotEnvGitEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-gitconfig"))
}

func dotenvGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeDotEnvFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
