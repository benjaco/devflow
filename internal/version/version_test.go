package version

import (
	"runtime"
	"runtime/debug"
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

func TestVersionFromBuildInfo(t *testing.T) {
	for _, test := range []struct {
		name  string
		build *debug.BuildInfo
		want  Info
	}{
		{
			name: "missing build info",
			want: Info{Version: "devel", ModulePath: ModulePath, GoVersion: runtime.Version()},
		},
		{
			name: "installed Devflow main module",
			build: &debug.BuildInfo{
				Main: debug.Module{Path: ModulePath, Version: "v0.1.2"},
			},
			want: Info{Version: "v0.1.2", ModulePath: ModulePath, GoVersion: runtime.Version()},
		},
		{
			name: "Devflow source checkout owns VCS metadata",
			build: &debug.BuildInfo{
				Main: debug.Module{Path: ModulePath, Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "devflow-revision"},
					{Key: "vcs.time", Value: "2026-09-10T00:00:00Z"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			want: Info{Version: "devel", ModulePath: ModulePath, GoVersion: runtime.Version(), VCSRevision: "devflow-revision", VCSTime: "2026-09-10T00:00:00Z", Modified: true},
		},
		{
			name: "generated module uses Devflow dependency and ignores app VCS",
			build: &debug.BuildInfo{
				Main: debug.Module{Path: ModulePath + "/localbuild/project", Version: "(devel)"},
				Deps: []*debug.Module{
					{Path: "example.com/other", Version: "v5.0.0"},
					{Path: ModulePath, Version: "v0.0.0-20260909213016-9616a60eaab7"},
				},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "app-revision"},
					{Key: "vcs.time", Value: "2026-09-10T01:00:00Z"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			want: Info{Version: "v0.0.0-20260909213016-9616a60eaab7", ModulePath: ModulePath, GoVersion: runtime.Version()},
		},
		{
			name: "versioned app does not supply Devflow version",
			build: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/app", Version: "v9.0.0"},
				Deps: []*debug.Module{{Path: ModulePath, Version: "v0.2.0"}},
			},
			want: Info{Version: "v0.2.0", ModulePath: ModulePath, GoVersion: runtime.Version()},
		},
		{
			name: "versioned dependency replacement identifies effective module",
			build: &debug.BuildInfo{
				Main: debug.Module{Path: ModulePath + "/runtime/project", Version: "(devel)"},
				Deps: []*debug.Module{{
					Path: ModulePath, Version: "v0.2.0",
					Replace: &debug.Module{Path: "example.com/devflow-fork", Version: "v0.3.0"},
				}},
			},
			want: Info{Version: "v0.3.0", ModulePath: "example.com/devflow-fork", GoVersion: runtime.Version()},
		},
		{
			name: "selected command uses main module replacement",
			build: &debug.BuildInfo{
				Main: debug.Module{
					Path: ModulePath, Version: "v0.2.0",
					Replace: &debug.Module{Path: "example.com/devflow-fork", Version: "v0.3.0"},
				},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "app-revision"}},
			},
			want: Info{Version: "v0.3.0", ModulePath: "example.com/devflow-fork", GoVersion: runtime.Version()},
		},
		{
			name: "selected command from local source reports development code",
			build: &debug.BuildInfo{
				Main: debug.Module{
					Path: ModulePath, Version: "v0.2.0",
					Replace: &debug.Module{Path: "../devflow"},
				},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "app-revision"}},
			},
			want: Info{Version: "devel", ModulePath: ModulePath, GoVersion: runtime.Version()},
		},
		{
			name: "local replacement cannot claim required release or app VCS",
			build: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/app", Version: "v9.0.0"},
				Deps: []*debug.Module{{
					Path: ModulePath, Version: "v0.2.0",
					Replace: &debug.Module{Path: "../devflow"},
				}},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "app-revision"}},
			},
			want: Info{Version: "devel", ModulePath: ModulePath, GoVersion: runtime.Version()},
		},
		{
			name: "unrelated build info cannot identify Devflow release",
			build: &debug.BuildInfo{
				Main:     debug.Module{Path: "example.com/app", Version: "v9.0.0"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "app-revision"}},
			},
			want: Info{Version: "devel", ModulePath: ModulePath, GoVersion: runtime.Version()},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := fromBuildInfo(test.build); got != test.want {
				t.Fatalf("Devflow version = %+v; want %+v", got, test.want)
			}
		})
	}
}
