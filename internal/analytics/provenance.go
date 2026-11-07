package analytics

import "runtime/debug"

// Revision returns the git commit the binary was built from, with "+dirty" when
// the tree had uncommitted changes, or "unknown" for a build without VCS
// stamping, such as go run or go test.
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
