package cli

import (
	"regexp"
	"runtime/debug"
	"strings"
)

// Version is the release version stamped in by the linker during a goreleaser
// build. Builds that skip that flag leave it empty and fall back to the module
// version Go records in the binary.
var Version = ""

const developmentVersion = "dev"

// pseudoVersionEnding closes every version Go derives for a commit instead of a
// tag: a UTC build timestamp and the commit's twelve-character prefix. Matching
// the ending rather than the opening keeps working once hop is tagged — before
// the first tag Go stamps v0.0.0-<stamp>, after it v1.4.1-0.<stamp>, which is
// why the timestamp may follow either a dash or a dot.
var pseudoVersionEnding = regexp.MustCompile(`[-.][0-9]{14}-[0-9a-f]{12}$`)

// dirtyBuildSuffix is what Go appends when the checkout carried uncommitted
// changes, which no release archive ever does.
const dirtyBuildSuffix = "+dirty"

func version() string {
	return resolveVersion(Version, moduleVersion())
}

func moduleVersion() string {
	info, readable := debug.ReadBuildInfo()
	if !readable {
		return ""
	}
	return info.Main.Version
}

// resolveVersion prefers the stamped release version, then the module version
// Go records for a binary installed with `go install ...@version`. Anything that
// names nothing anyone could install reads as a development build instead.
func resolveVersion(stamped, module string) string {
	if stamped != "" {
		return stamped
	}
	if namesAnInstallableRelease(module) {
		return module
	}
	return developmentVersion
}

// namesAnInstallableRelease reports whether a module version identifies a
// release someone could install, rather than somebody's checkout. Go records
// "(devel)" when it cannot read the checkout at all, a pseudo-version when it
// can read it but the commit carries no release tag, and a +dirty suffix when
// the tree had uncommitted changes.
func namesAnInstallableRelease(module string) bool {
	if module == "" || module == "(devel)" {
		return false
	}
	if strings.HasSuffix(module, dirtyBuildSuffix) {
		return false
	}
	return !pseudoVersionEnding.MatchString(module)
}
