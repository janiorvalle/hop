package claude

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestLiveUsageReadOnly(t *testing.T) {
	if os.Getenv("HOP_CLAUDE_LIVE_TEST") != "1" {
		t.Skip("set HOP_CLAUDE_LIVE_TEST=1 to run one read-only usage request")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	credentials, err := ReadLiveCredentials(ctx)
	if err != nil {
		t.Fatalf("ReadLiveCredentials() error = %v", err)
	}
	usage, err := New(Config{}).FetchUsage(ctx, credentials)
	if err != nil {
		t.Fatalf("FetchUsage() error = %v", err)
	}
	receipt, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	t.Logf("read-only normalized usage receipt: %s", receipt)
}
