package version

import "runtime/debug"

// Overridden at build time:
//
//	go build -ldflags "-X github.com/cti97/the-nightshift-manager/internal/version.Version=v0.1.0"
var (
	Version = "dev"
	Commit  = "unknown"
)

// Info returns the version and the commit. When ldflags did not set a commit,
// the toolchain's own VCS stamp is used, so a local build can still name the
// commit it came from (§20 of the spec).
func Info() (string, string) {
	c := Commit
	if c == "unknown" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && s.Value != "" {
					c = s.Value
					if len(c) > 12 {
						c = c[:12]
					}
				}
			}
		}
	}
	return Version, c
}

func String() string {
	v, c := Info()
	if c == "unknown" {
		return v
	}
	return v + " (" + c + ")"
}
