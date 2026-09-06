package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/janiorvalle/hop/internal/provider"
	"github.com/janiorvalle/hop/internal/render"
)

type slotRotator interface {
	Rotate(context.Context) (rotation, error)
}

func refreshAccounts(ctx context.Context, stdout io.Writer) error {
	accountCatalog, err := defaultCatalog()
	if err != nil {
		return err
	}
	return refreshAccountsFrom(ctx, stdout, accountCatalog)
}

func refreshAccountsFrom(ctx context.Context, stdout io.Writer, accountCatalog catalog) error {
	accounts, err := accountCatalog.Accounts()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		_, err := io.WriteString(stdout, render.NoAccountsEnrolled)
		return err
	}
	failed := 0
	for _, currentAccount := range accounts {
		status, err := refreshStatus(ctx, currentAccount)
		if err != nil {
			failed++
			status = "failed: " + strings.ReplaceAll(err.Error(), "<account>", currentAccount.Name)
		}
		if _, err := fmt.Fprintf(stdout, "%s %s: %s\n", currentAccount.Provider, currentAccount.Name, status); err != nil {
			return err
		}
	}
	if failed == 0 {
		return nil
	}
	return fmt.Errorf("[REFRESH_FAILED] %d of %d accounts could not be rotated. Follow the next step on each 'failed' line above, then run 'hop refresh' again", failed, len(accounts))
}

func refreshStatus(ctx context.Context, current account) (string, error) {
	if current.Disabled {
		return "skipped: disabled, run 'hop enable " + string(current.Provider) + " " + current.Name + "' to rotate it again", nil
	}
	if current.Active {
		return "skipped: active account, hop never rotates the live login", nil
	}
	rotator, ok := current.Source.(slotRotator)
	if !ok {
		return unmanagedStatus(current), nil
	}
	outcome, err := rotator.Rotate(ctx)
	if err != nil {
		return "", err
	}
	switch outcome.Outcome {
	case rotationRotated:
		if outcome.RefreshTokenExpiry.IsZero() {
			return "rotated, the provider did not say when the refresh token expires", nil
		}
		return "rotated, refresh token good until " + outcome.RefreshTokenExpiry.UTC().Format("2006-01-02"), nil
	case rotationFresh:
		return "fresh, no rotation needed", nil
	case rotationBecameActive:
		return "skipped: became active, hop never rotates the live login", nil
	default:
		return unmanagedStatus(current), nil
	}
}

var unmanagedRemedies = map[provider.Name]string{
	provider.Claude: "run 'hop rm claude %[1]s' and then 'hop login claude %[1]s'",
	provider.Codex:  "run 'hop login codex %[1]s'",
}

func unmanagedStatus(current account) string {
	return fmt.Sprintf("skipped: not managed by hop, %s to let hop rotate it", fmt.Sprintf(unmanagedRemedies[current.Provider], current.Name))
}
