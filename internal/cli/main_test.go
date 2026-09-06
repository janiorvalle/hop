package cli

import (
	"os"
	"testing"
)

// A command-level test that reaches hop's Claude login once opened the real
// browser and finished a real sign-in, because the public client id is
// pre-authorized on a signed-in account. No test gets a browser.
func TestMain(m *testing.M) {
	_ = os.Setenv("BROWSER", "hop-tests-never-open-a-browser")
	os.Exit(m.Run())
}
