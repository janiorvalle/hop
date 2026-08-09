package cli

import "runtime/debug"

// Version is the release version stamped in by the linker during a goreleaser
// build. Builds that skip that flag leave it empty and fall back to the module
// version Go records in the binary.
var Version = ""

const developmentVersion = "dev"

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
// Go records for a binary installed with `go install ...@version`. Go reports
// "(devel)" for a build from a working tree, which names nothing anyone could
// install, so that case reads as a development build.
func resolveVersion(stamped, module string) string {
	if stamped != "" {
		return stamped
	}
	if module != "" && module != "(devel)" {
		return module
	}
	return developmentVersion
}
