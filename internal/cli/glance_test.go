package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

type staticCatalog []account

func (catalog staticCatalog) Accounts() ([]account, error) {
	return []account(catalog), nil
}

type fetchFunc func(context.Context) (provider.Usage, error)

func (fetch fetchFunc) FetchUsage(ctx context.Context) (provider.Usage, error) {
	return fetch(ctx)
}

type preparingFetcher struct {
	prepare func(context.Context) error
	fetch   fetchFunc
}

func (fetcher preparingFetcher) Prepare(ctx context.Context) error {
	return fetcher.prepare(ctx)
}

func (fetcher preparingFetcher) FetchUsage(ctx context.Context) (provider.Usage, error) {
	return fetcher.fetch(ctx)
}

func TestFetchGlanceRunsAccountsInParallelAndIsolatesErrors(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	blocking := func(usage provider.Usage, err error) provider.Fetcher {
		return fetchFunc(func(context.Context) (provider.Usage, error) {
			started <- struct{}{}
			<-release
			return usage, err
		})
	}
	catalog := staticCatalog{
		{Provider: provider.Claude, Name: "work", Active: true, Fetcher: blocking(provider.Usage{Provider: provider.Claude, Windows: []provider.Window{}}, nil)},
		{Provider: provider.Codex, Name: "broken", Fetcher: blocking(provider.Usage{}, errors.New("token expired; refresh the account token and retry"))},
	}
	type response struct {
		document glanceDocument
		err      error
	}
	completed := make(chan response, 1)
	go func() {
		document, err := fetchGlance(context.Background(), catalog)
		completed <- response{document: document, err: err}
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("fetchers did not start together; glance is not parallel")
		}
	}
	close(release)
	result := <-completed
	if result.err != nil {
		t.Fatalf("fetchGlance() error = %v", result.err)
	}
	if result.document.Schema != listSchema || len(result.document.Accounts) != 2 {
		t.Fatalf("document = %+v", result.document)
	}
	if result.document.Accounts[0].Account != "work" || result.document.Accounts[0].Error != nil {
		t.Errorf("first account = %+v, want successful work", result.document.Accounts[0])
	}
	problem := result.document.Accounts[1].Error
	if problem == nil || problem.Code != "USAGE_UNAVAILABLE" || problem.Action != "token expired; refresh the account token and retry" {
		t.Fatalf("broken account problem = %+v", problem)
	}
}

func TestShowAccountsIncludesUnderlyingUsageError(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{{
		Provider: provider.Claude,
		Name:     "work2",
		Fetcher: fetchFunc(func(context.Context) (provider.Usage, error) {
			return provider.Usage{}, errors.New("Claude usage returned HTTP 429; wait and retry")
		}),
	}}
	for _, asJSON := range []bool{false, true} {
		var output bytes.Buffer
		if err := showAccountsFrom(context.Background(), &output, asJSON, catalog, time.Now()); err != nil {
			t.Fatalf("showAccountsFrom(asJSON=%t) error = %v", asJSON, err)
		}
		unwrapped := strings.Join(strings.Fields(output.String()), " ")
		if !strings.Contains(unwrapped, "Claude usage returned HTTP 429; wait and retry") {
			t.Fatalf("showAccountsFrom(asJSON=%t) output omitted cause: %s", asJSON, output.String())
		}
		if bytes.Contains(output.Bytes(), []byte("hop login")) {
			t.Fatalf("showAccountsFrom(asJSON=%t) output gave unrelated login advice: %s", asJSON, output.String())
		}
	}
}

func TestShowAccountsSubstitutesTheFailedAccountInRecoveryCommands(t *testing.T) {
	t.Parallel()

	problem := usageProblem(account{Provider: provider.Claude, Name: "work2"}, errors.New("access token is missing; run 'hop login claude <account>'"))
	if strings.Contains(problem.Action, "<account>") || !strings.Contains(problem.Action, "hop login claude work2") {
		t.Fatalf("usage problem action = %q, want runnable work2 command", problem.Action)
	}
}

func TestFetchGlanceDoesNotBlockHealthyAccountBehindSlowPreparation(t *testing.T) {
	t.Parallel()

	preparationStarted := make(chan struct{})
	releasePreparation := make(chan struct{})
	healthyFetched := make(chan struct{})
	catalog := staticCatalog{
		{
			Provider: provider.Claude,
			Name:     "refreshing",
			Fetcher: preparingFetcher{
				prepare: func(context.Context) error {
					close(preparationStarted)
					<-releasePreparation
					return nil
				},
				fetch: func(context.Context) (provider.Usage, error) {
					return provider.Usage{Provider: provider.Claude}, nil
				},
			},
		},
		{
			Provider: provider.Codex,
			Name:     "healthy",
			Fetcher: fetchFunc(func(context.Context) (provider.Usage, error) {
				close(healthyFetched)
				return provider.Usage{Provider: provider.Codex}, nil
			}),
		},
	}
	completed := make(chan error, 1)
	go func() {
		_, err := fetchGlance(context.Background(), catalog)
		completed <- err
	}()
	<-preparationStarted
	select {
	case <-healthyFetched:
	case <-time.After(time.Second):
		t.Fatal("healthy account waited behind another account's preparation")
	}
	close(releasePreparation)
	if err := <-completed; err != nil {
		t.Fatalf("fetchGlance() error = %v", err)
	}
}

func TestShowAccountsJSONHasStableSchemaAndEmptyArrays(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{{
		Provider: provider.Codex,
		Name:     "work",
		Fetcher: fetchFunc(func(context.Context) (provider.Usage, error) {
			return provider.Usage{Provider: provider.Codex, Email: "owner@example.com", Plan: "pro"}, nil
		}),
	}}
	var output bytes.Buffer
	if err := showAccountsFrom(context.Background(), &output, true, catalog, time.Now()); err != nil {
		t.Fatalf("showAccountsFrom() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("JSON output is invalid: %v\n%s", err, output.String())
	}
	if document["schema"] != "hop.ls/v1" {
		t.Fatalf("schema = %v, want hop.ls/v1", document["schema"])
	}
	accounts, ok := document["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts = %#v, want one", document["accounts"])
	}
	account := accounts[0].(map[string]any)
	if _, ok := account["windows"].([]any); !ok {
		t.Fatalf("windows = %#v, want JSON array", account["windows"])
	}
	if _, ok := account["limits"].([]any); !ok {
		t.Fatalf("limits = %#v, want JSON array", account["limits"])
	}
}

func TestShowAccountsJSONOmitsMissingResets(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{{
		Provider: provider.Claude,
		Name:     "idle",
		Fetcher: fetchFunc(func(context.Context) (provider.Usage, error) {
			return provider.Usage{
				Provider: provider.Claude,
				Windows:  []provider.Window{{Kind: provider.FiveHour, UsedPercent: 0}},
				Limits:   []provider.Limit{{Kind: "session", Group: "session", UsedPercent: 0}},
			}, nil
		}),
	}}
	var output bytes.Buffer
	if err := showAccountsFrom(context.Background(), &output, true, catalog, time.Now()); err != nil {
		t.Fatalf("showAccountsFrom() error = %v", err)
	}
	if bytes.Contains(output.Bytes(), []byte(`"resets_at"`)) {
		t.Fatalf("JSON exposed absent reset fields: %s", output.String())
	}
	if bytes.Contains(output.Bytes(), []byte("0001-01-01")) {
		t.Fatalf("JSON exposed a year-one reset: %s", output.String())
	}
}

func TestShowAccountsEmptyCatalogGivesEnrollmentStep(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if err := showAccountsFrom(context.Background(), &output, false, staticCatalog{}, time.Now()); err != nil {
		t.Fatalf("showAccountsFrom() error = %v", err)
	}
	if got := output.String(); got != "No accounts enrolled. Run 'hop login claude work' or 'hop login codex work'.\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestShowAccountsHonorsAnEarlierCallerDeadline(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{{
		Provider: provider.Claude,
		Name:     "offline",
		Fetcher: fetchFunc(func(ctx context.Context) (provider.Usage, error) {
			<-ctx.Done()
			return provider.Usage{}, ctx.Err()
		}),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	if err := showAccountsFrom(ctx, &output, true, catalog, time.Now()); err != nil {
		t.Fatalf("showAccountsFrom() error = %v", err)
	}
	var document glanceDocument
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(document.Accounts) != 1 || document.Accounts[0].Error == nil {
		t.Fatalf("accounts = %+v, want one isolated timeout error", document.Accounts)
	}
}
