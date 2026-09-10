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
	build, _ := debug.ReadBuildInfo()
	return fromBuildInfo(build)
}

func fromBuildInfo(build *debug.BuildInfo) Info {
	info := Info{
		Version:    "devel",
		ModulePath: ModulePath,
		GoVersion:  runtime.Version(),
	}
	if build == nil {
		return info
	}
	module := &build.Main
	if module.Path != ModulePath {
		module = nil
		for _, dependency := range build.Deps {
			if dependency.Path == ModulePath {
				module = dependency
				break
			}
		}
	}
	if module == nil {
		return info
	}
	if module.Replace != nil {
		module = module.Replace
		// A local checkout has no release identity, regardless of its require line.
		if module.Version == "" || module.Version == "(devel)" {
			return info
		}
		info.ModulePath = module.Path
	}
	if module.Version != "" && module.Version != "(devel)" {
		info.Version = module.Version
	}
	// Generated launchers/adapters carry the application's VCS settings, not ours.
	if build.Main.Path != ModulePath || build.Main.Replace != nil {
		return info
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
	return info
}
