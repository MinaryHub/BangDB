package bangdb

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Version, Commit and BuildTime describe the build. The build script
// (scripts/build.sh) sets them with -ldflags "-X ...": Version is the base
// version in the VERSION file followed by the number of commits, so every
// build of a new commit gets a higher version, e.g. 0.6.43. A plain
// "go build" leaves Version as "dev" and VersionString falls back to the
// version control data Go records in the binary.
var (
	Version   = "dev"
	Commit    = ""
	BuildTime = ""
)

// VersionString returns a one-line description of the build, such as
// "0.6.43 (a94717d, 2026-10-26T23:30:00Z)".
func VersionString() string {
	v, commit, when, dirty := Version, Commit, BuildTime, false
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = strings.TrimPrefix(info.Main.Version, "v") // go install ...@version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "" {
					commit = s.Value
				}
			case "vcs.time":
				if when == "" {
					when = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true" && Commit == ""
			}
		}
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	var details []string
	if commit != "" {
		if dirty {
			commit += "-dirty"
		}
		details = append(details, commit)
	}
	if when != "" {
		details = append(details, when)
	}
	if len(details) == 0 {
		return v
	}
	return fmt.Sprintf("%s (%s)", v, strings.Join(details, ", "))
}
