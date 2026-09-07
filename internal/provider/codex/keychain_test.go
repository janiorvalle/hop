package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Codex keeps its MCP logins in Keychain items of their own, apart from
// auth.json. hop swaps only auth.json, so nothing in this package may reach
// for the Keychain at all.
func TestCodexPackageNeverNamesTheKeychain(t *testing.T) {
	t.Parallel()

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		contents, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"Codex MCP Credentials", "generic-password", `"security"`} {
			if strings.Contains(string(contents), forbidden) {
				t.Errorf("%s mentions %q; Codex MCP logins live in the Keychain and hop must leave them alone", source, forbidden)
			}
		}
	}
}
