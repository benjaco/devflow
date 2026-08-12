package version

import (
	"runtime"
	"testing"
)

func TestCurrentHasStableIdentity(t *testing.T) {
	got := Current()
	if got.ModulePath != ModulePath {
		t.Fatalf("module path = %q, want %q", got.ModulePath, ModulePath)
	}
	if got.Version == "" {
		t.Fatal("version is empty")
	}
	if got.GoVersion != runtime.Version() {
		t.Fatalf("Go version = %q, want %q", got.GoVersion, runtime.Version())
	}
}
