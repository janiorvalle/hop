package cli

import (
	"bytes"
	"testing"
	"time"
)

func TestTerminalOptionsHonorColumnsAndNoColor(t *testing.T) {
	t.Setenv("COLUMNS", "64")
	t.Setenv("NO_COLOR", "1")

	now := time.Date(2026, time.August, 8, 6, 0, 0, 0, time.UTC)
	options := terminalOptions(&bytes.Buffer{}, now)
	if options.Width != 64 {
		t.Errorf("width = %d, want 64", options.Width)
	}
	if options.Color || !options.Plain {
		t.Errorf("color/plain = %t/%t, want false/true", options.Color, options.Plain)
	}
	if !options.Now.Equal(now) {
		t.Errorf("now = %s, want %s", options.Now, now)
	}
}
