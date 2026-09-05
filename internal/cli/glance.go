package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

const listSchema = "hop.ls/v1"

const usageTimeout = 5 * time.Second

type glanceDocument struct {
	Schema   string          `json:"schema"`
	Accounts []accountResult `json:"accounts"`
}

type accountResult struct {
	Provider     provider.Name          `json:"provider"`
	Account      string                 `json:"account"`
	Active       bool                   `json:"active"`
	Disabled     bool                   `json:"disabled"`
	Email        string                 `json:"email,omitempty"`
	Plan         string                 `json:"plan,omitempty"`
	Windows      []provider.Window      `json:"windows"`
	Limits       []provider.Limit       `json:"limits"`
	ResetCredits *provider.ResetCredits `json:"reset_credits,omitempty"`
	Error        *accountProblem        `json:"error,omitempty"`
	// RefreshTokenExpiry is additive on hop.ls/v1 and absent when the expiry is unknown.
	RefreshTokenExpiry *refreshTokenExpiry `json:"refresh_token_expiry,omitempty"`
}

type refreshTokenExpiry struct {
	ExpiresAt time.Time `json:"expires_at"`
	Severity  string    `json:"severity"`
	Action    string    `json:"action,omitempty"`
}

type accountProblem struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Action    string `json:"action"`
	Retryable bool   `json:"retryable"`
}

type indexedResult struct {
	index  int
	result accountResult
}

type accountPreparer interface {
	Prepare(context.Context) error
}

func fetchGlance(ctx context.Context, accountCatalog catalog, now time.Time) (glanceDocument, error) {
	accounts, err := accountCatalog.Accounts()
	if err != nil {
		return glanceDocument{}, err
	}
	document := glanceDocument{Schema: listSchema, Accounts: make([]accountResult, len(accounts))}
	results := make(chan indexedResult, len(accounts))
	fetching := 0
	for index, currentAccount := range accounts {
		if currentAccount.Disabled {
			document.Accounts[index] = resultFor(currentAccount, provider.Usage{}, nil)
			continue
		}
		fetching++
		go func() {
			if preparer, ok := currentAccount.Fetcher.(accountPreparer); ok {
				if err := preparer.Prepare(ctx); err != nil {
					results <- indexedResult{index: index, result: resultFor(currentAccount, provider.Usage{}, err)}
					return
				}
			}
			usageCtx, cancel := context.WithTimeout(ctx, usageTimeout)
			defer cancel()
			usage, fetchErr := currentAccount.Fetcher.FetchUsage(usageCtx)
			result := resultFor(currentAccount, usage, fetchErr)
			result.RefreshTokenExpiry = refreshTokenExpiryFor(currentAccount, usage.RefreshTokenExpiresAt, now)
			results <- indexedResult{index: index, result: result}
		}()
	}
	for range fetching {
		result := <-results
		document.Accounts[result.index] = result.result
	}
	return document, nil
}

func resultFor(account account, usage provider.Usage, err error) accountResult {
	result := accountResult{
		Provider: account.Provider,
		Account:  account.Name,
		Active:   account.Active,
		Disabled: account.Disabled,
		Windows:  make([]provider.Window, 0),
		Limits:   make([]provider.Limit, 0),
	}
	if err != nil {
		result.Error = usageProblem(account, err)
		return result
	}
	result.Email = usage.Email
	result.Plan = usage.Plan
	result.Windows = usage.Windows
	result.Limits = usage.Limits
	if result.Windows == nil {
		result.Windows = make([]provider.Window, 0)
	}
	if result.Limits == nil {
		result.Limits = make([]provider.Limit, 0)
	}
	if usage.ResetCredits != nil {
		credits := *usage.ResetCredits
		if credits.Credits == nil {
			credits.Credits = make([]provider.ResetCredit, 0)
		}
		result.ResetCredits = &credits
	}
	return result
}

// Only the provider CLI can renew the live login; hop login would adopt it unchanged.
var liveRenewalActions = map[provider.Name]string{
	provider.Claude: "Run 'claude' and use /login to renew it.",
	provider.Codex:  "Run 'codex login' to renew it.",
}

func refreshTokenExpiryFor(account account, expiresAt, now time.Time) *refreshTokenExpiry {
	if expiresAt.IsZero() {
		return nil
	}
	expiry := &refreshTokenExpiry{ExpiresAt: expiresAt, Severity: refreshTokenSeverity(expiresAt, now)}
	if expiry.Severity == "normal" {
		return expiry
	}
	expiry.Action = fmt.Sprintf("Run 'hop rm %s %s' and then 'hop login %s %s' to renew it.", account.Provider, account.Name, account.Provider, account.Name)
	if account.Active {
		expiry.Action = liveRenewalActions[account.Provider]
	}
	return expiry
}

func usageProblem(failedAccount account, err error) *accountProblem {
	return &accountProblem{
		Code:      "USAGE_UNAVAILABLE",
		Message:   fmt.Sprintf("Usage could not be loaded for %s account %q.", failedAccount.Provider, failedAccount.Name),
		Action:    strings.ReplaceAll(err.Error(), "<account>", failedAccount.Name),
		Retryable: true,
	}
}
