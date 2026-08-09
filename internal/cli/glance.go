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
	Provider provider.Name     `json:"provider"`
	Account  string            `json:"account"`
	Active   bool              `json:"active"`
	Email    string            `json:"email,omitempty"`
	Plan     string            `json:"plan,omitempty"`
	Windows  []provider.Window `json:"windows"`
	Limits   []provider.Limit  `json:"limits"`
	Error    *accountProblem   `json:"error,omitempty"`
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

func fetchGlance(ctx context.Context, accountCatalog catalog) (glanceDocument, error) {
	accounts, err := accountCatalog.Accounts()
	if err != nil {
		return glanceDocument{}, err
	}
	document := glanceDocument{Schema: listSchema, Accounts: make([]accountResult, len(accounts))}
	results := make(chan indexedResult, len(accounts))
	for index, currentAccount := range accounts {
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
			results <- indexedResult{index: index, result: resultFor(currentAccount, usage, fetchErr)}
		}()
	}
	for range accounts {
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
	return result
}

func usageProblem(failedAccount account, err error) *accountProblem {
	return &accountProblem{
		Code:      "USAGE_UNAVAILABLE",
		Message:   fmt.Sprintf("Usage could not be loaded for %s account %q.", failedAccount.Provider, failedAccount.Name),
		Action:    strings.ReplaceAll(err.Error(), "<account>", failedAccount.Name),
		Retryable: true,
	}
}
