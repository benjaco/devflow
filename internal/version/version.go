package version

import (
	"runtime"
	"runtime/debug"
)

const (
	ModulePath     = "github.com/benjaco/devflow"
	CommandPackage = ModulePath + "/cmd/devflow"
)

type Info struct {
	Version     string `json:"version"`
	ModulePath  string `json:"modulePath"`
	GoVersion   string `json:"goVersion"`
	VCSRevision string `json:"vcsRevision,omitempty"`
	VCSTime     string `json:"vcsTime,omitempty"`
	Modified    bool   `json:"modified,omitempty"`
}

func Current() Info {
	info := Info{
		Version:    "devel",
		ModulePath: ModulePath,
		GoVersion:  runtime.Version(),
	}
	if build, ok := debug.ReadBuildInfo(); ok {
		if build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Version = build.Main.Version
		}
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				info.VCSRevision = setting.Value
			case "vcs.time":
				info.VCSTime = setting.Value
			case "vcs.modified":
				info.Modified = setting.Value == "true"
			}
		}
	}
	return info
}
