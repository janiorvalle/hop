package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

type staticCatalog []account

func (catalog staticCatalog) Accounts() ([]account, error) {
	return []account(catalog), nil
}

// fetchFunc is a source whose credentials say nothing and whose fetch is the function.
type fetchFunc func(context.Context) (provider.Usage, error)

func (fetch fetchFunc) Open(context.Context) (provider.Fetcher, error) {
	return fetch, nil
}

func (fetch fetchFunc) Enrollment() provider.Enrollment {
	return provider.Enrollment{}
}

func (fetch fetchFunc) FetchUsage(ctx context.Context) (provider.Usage, error) {
	return fetch(ctx)
}

// enrolledSource is a source whose credentials name a plan and an expiry.
type enrolledSource struct {
	enrollment provider.Enrollment
	fetch      fetchFunc
}

func (source enrolledSource) Open(context.Context) (provider.Fetcher, error) {
	return source, nil
}

func (source enrolledSource) Enrollment() provider.Enrollment {
	return source.enrollment
}

func (source enrolledSource) FetchUsage(ctx context.Context) (provider.Usage, error) {
	return source.fetch(ctx)
}

type preparingSource struct {
	prepare func(context.Context) error
	fetch   fetchFunc
}

func (source preparingSource) Prepare(ctx context.Context) error {
	return source.prepare(ctx)
}

func (source preparingSource) Open(context.Context) (provider.Fetcher, error) {
	return source.fetch, nil
}

func TestFetchGlanceRunsAccountsInParallelAndIsolatesErrors(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	blocking := func(usage provider.Usage, err error) provider.Source {
		return fetchFunc(func(context.Context) (provider.Usage, error) {
			started <- struct{}{}
			<-release
			return usage, err
		})
	}
	catalog := staticCatalog{
		{Provider: provider.Claude, Name: "work", Active: true, Source: blocking(provider.Usage{Provider: provider.Claude, Windows: []provider.Window{}}, nil)},
		{Provider: provider.Codex, Name: "broken", Source: blocking(provider.Usage{}, errors.New("token expired; refresh the account token and retry"))},
	}
	type response struct {
		document glanceDocument
		err      error
	}
	completed := make(chan response, 1)
	go func() {
		document, err := fetchGlance(context.Background(), catalog, time.Now())
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
		Source: fetchFunc(func(context.Context) (provider.Usage, error) {
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
			Source: preparingSource{
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
			Source: fetchFunc(func(context.Context) (provider.Usage, error) {
				close(healthyFetched)
				return provider.Usage{Provider: provider.Codex}, nil
			}),
		},
	}
	completed := make(chan error, 1)
	go func() {
		_, err := fetchGlance(context.Background(), catalog, time.Now())
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
		Source: fetchFunc(func(context.Context) (provider.Usage, error) {
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

func TestShowAccountsJSONCarriesResetCreditsOnlyForCodex(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{
		{
			Provider: provider.Codex,
			Name:     "work",
			Source: fetchFunc(func(context.Context) (provider.Usage, error) {
				return provider.Usage{Provider: provider.Codex, ResetCredits: &provider.ResetCredits{Count: 1}}, nil
			}),
		},
		{
			Provider: provider.Claude,
			Name:     "work",
			Source: fetchFunc(func(context.Context) (provider.Usage, error) {
				return provider.Usage{Provider: provider.Claude}, nil
			}),
		},
	}
	var output bytes.Buffer
	if err := showAccountsFrom(context.Background(), &output, true, catalog, time.Now()); err != nil {
		t.Fatalf("showAccountsFrom() error = %v", err)
	}
	var document struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("JSON output is invalid: %v\n%s", err, output.String())
	}
	codex, ok := document.Accounts[0]["reset_credits"].(map[string]any)
	if !ok || codex["count"] != float64(1) {
		t.Fatalf("codex reset_credits = %#v, want count 1", document.Accounts[0]["reset_credits"])
	}
	if _, ok := codex["credits"].([]any); !ok {
		t.Fatalf("codex credits = %#v, want a JSON array even when the fetcher gave none", codex["credits"])
	}
	if _, present := document.Accounts[1]["reset_credits"]; present {
		t.Fatalf("claude row carries reset_credits: %s", output.String())
	}
}

func TestShowAccountsJSONOmitsMissingResets(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{{
		Provider: provider.Claude,
		Name:     "idle",
		Source: fetchFunc(func(context.Context) (provider.Usage, error) {
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
		Source: fetchFunc(func(ctx context.Context) (provider.Usage, error) {
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

func TestFetchGlanceWarnsAtRefreshTokenThresholds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name         string
		provider     provider.Name
		expiresAt    time.Time
		active       bool
		wantSeverity string
		wantAction   string
	}{
		{name: "thirty days out", provider: provider.Claude, expiresAt: now.Add(30 * 24 * time.Hour), wantSeverity: "normal"},
		{name: "six days out", provider: provider.Claude, expiresAt: now.Add(6 * 24 * time.Hour), wantSeverity: "warning", wantAction: "Run 'hop rm claude work' and then 'hop login claude work' to renew it."},
		{name: "one day out", provider: provider.Claude, expiresAt: now.Add(24 * time.Hour), wantSeverity: "critical", wantAction: "Run 'hop rm claude work' and then 'hop login claude work' to renew it."},
		{name: "codex slot renews in place", provider: provider.Codex, expiresAt: now.Add(24 * time.Hour), wantSeverity: "critical", wantAction: "Run 'hop login codex work' to renew it."},
		{name: "active account renews through its own CLI", provider: provider.Claude, expiresAt: now.Add(24 * time.Hour), active: true, wantSeverity: "critical", wantAction: "Run 'claude' and use /login to renew it."},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			catalog := staticCatalog{{Provider: scenario.provider, Name: "work", Active: scenario.active, Source: enrolledSource{
				enrollment: provider.Enrollment{RefreshTokenExpiresAt: scenario.expiresAt},
				fetch:      func(context.Context) (provider.Usage, error) { return provider.Usage{Provider: scenario.provider}, nil },
			}}}
			document, err := fetchGlance(context.Background(), catalog, now)
			if err != nil {
				t.Fatalf("fetchGlance() error = %v", err)
			}
			expiry := document.Accounts[0].RefreshTokenExpiry
			if expiry == nil {
				t.Fatal("refresh_token_expiry = nil, want a status")
			}
			if !expiry.ExpiresAt.Equal(scenario.expiresAt) || expiry.Severity != scenario.wantSeverity || expiry.Action != scenario.wantAction {
				t.Fatalf("refresh_token_expiry = %+v, want %s at %s with action %q", expiry, scenario.wantSeverity, scenario.expiresAt, scenario.wantAction)
			}
		})
	}
}

func TestFetchGlanceOmitsRefreshTokenExpiryWhenUnknown(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{{Provider: provider.Codex, Name: "seeded", Source: fetchFunc(func(context.Context) (provider.Usage, error) {
		return provider.Usage{Provider: provider.Codex}, nil
	})}}
	document, err := fetchGlance(context.Background(), catalog, time.Now())
	if err != nil {
		t.Fatalf("fetchGlance() error = %v", err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if bytes.Contains(encoded, []byte("refresh_token_expiry")) {
		t.Fatalf("JSON carries refresh_token_expiry for an unknown expiry: %s", encoded)
	}
}

func TestFetchGlanceKeepsEnrollmentOnErrorRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name         string
		expiresAt    time.Time
		wantSeverity string
	}{
		{name: "six days out", expiresAt: now.Add(6 * 24 * time.Hour), wantSeverity: "warning"},
		{name: "one day out", expiresAt: now.Add(24 * time.Hour), wantSeverity: "critical"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			catalog := staticCatalog{{Provider: provider.Claude, Name: "stale", Source: enrolledSource{
				enrollment: provider.Enrollment{Plan: "Pro", RefreshTokenExpiresAt: scenario.expiresAt},
				fetch: func(context.Context) (provider.Usage, error) {
					return provider.Usage{}, errors.New("claude usage returned HTTP 502; the provider usage service is unavailable, retry 'hop ls' later")
				},
			}}}
			document, err := fetchGlance(context.Background(), catalog, now)
			if err != nil {
				t.Fatalf("fetchGlance() error = %v", err)
			}
			row := document.Accounts[0]
			if row.Error == nil || row.Error.Code != "USAGE_UNAVAILABLE" {
				t.Fatalf("error = %+v, want USAGE_UNAVAILABLE", row.Error)
			}
			if row.Plan != "Pro" {
				t.Fatalf("plan = %q, want Pro kept from the credentials", row.Plan)
			}
			if row.RefreshTokenExpiry == nil || row.RefreshTokenExpiry.Severity != scenario.wantSeverity || row.RefreshTokenExpiry.Action == "" {
				t.Fatalf("refresh_token_expiry = %+v, want %s with an action", row.RefreshTokenExpiry, scenario.wantSeverity)
			}
			if len(row.Windows) != 0 || len(row.Limits) != 0 {
				t.Fatalf("error row carries usage: %+v", row)
			}
		})
	}
}

func TestFetchGlancePrefersThePlanTheUsageResponseNames(t *testing.T) {
	t.Parallel()

	catalog := staticCatalog{{Provider: provider.Codex, Name: "work", Source: enrolledSource{
		enrollment: provider.Enrollment{Plan: "plus"},
		fetch: func(context.Context) (provider.Usage, error) {
			return provider.Usage{Provider: provider.Codex, Plan: "pro"}, nil
		},
	}}}
	document, err := fetchGlance(context.Background(), catalog, time.Now())
	if err != nil {
		t.Fatalf("fetchGlance() error = %v", err)
	}
	if document.Accounts[0].Plan != "pro" {
		t.Fatalf("plan = %q, want the usage response's pro over the login-time plus", document.Accounts[0].Plan)
	}
}

func TestAttentionProblemKeepsTheStatusAndTheQuotedCommands(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		name         string
		action       string
		wantFact     string
		wantCommands []string
	}{
		{
			name:         "refresh failure names the login",
			action:       "claude token endpoint returned HTTP 400; the slot was not changed, run 'hop login claude work2': claude token refresh failed",
			wantFact:     "usage unavailable (HTTP 400)",
			wantCommands: []string{"hop login claude work2"},
		},
		{
			name:         "rate limit names the retry",
			action:       "codex usage returned HTTP 429; the provider rate-limited the request, wait and retry 'hop ls': codex usage request failed",
			wantFact:     "usage unavailable (HTTP 429)",
			wantCommands: []string{"hop ls"},
		},
		{
			name:         "a bare hop login is a hint, not a command",
			action:       "read slot metadata from /tmp/hop/claude/work/slot.json; expected {\"refresh_policy\":\"managed\"}, fix the file or run 'hop login' again: unexpected end of JSON input",
			wantFact:     "usage unavailable",
			wantCommands: []string{"read slot metadata from /tmp/hop/claude/work/slot.json; expected {\"refresh_policy\":\"managed\"}, fix the file or run 'hop login' again: unexpected end of JSON input"},
		},
		{
			name:         "no quoted command keeps the whole next step",
			action:       "decode Claude session limit; resets_at may be null only while percent is zero, update hop before retrying: claude usage request failed",
			wantFact:     "usage unavailable",
			wantCommands: []string{"decode Claude session limit; resets_at may be null only while percent is zero, update hop before retrying: claude usage request failed"},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			got := attentionProblem(accountProblem{Code: "USAGE_UNAVAILABLE", Action: scenario.action})
			if got.Fact != scenario.wantFact || !reflect.DeepEqual(got.Commands, scenario.wantCommands) {
				t.Fatalf("attentionProblem() = %+v, want fact %q and commands %q", got, scenario.wantFact, scenario.wantCommands)
			}
		})
	}
}

func TestRenewalCommandsMatchTheProseActions(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		name     string
		provider provider.Name
		active   bool
		want     []string
	}{
		{name: "claude slot", provider: provider.Claude, want: []string{"hop rm claude work", "hop login claude work"}},
		{name: "codex slot", provider: provider.Codex, want: []string{"hop login codex work"}},
		{name: "live claude", provider: provider.Claude, active: true, want: []string{"claude, then /login"}},
		{name: "live codex", provider: provider.Codex, active: true, want: []string{"codex login"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			if got := renewalCommands(scenario.provider, "work", scenario.active); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("renewalCommands() = %q, want %q", got, scenario.want)
			}
		})
	}
}

func TestShowAccountsTableListsTheFailureUnderTheRows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	catalog := staticCatalog{
		{Provider: provider.Claude, Name: "work", Source: fetchFunc(func(context.Context) (provider.Usage, error) {
			return provider.Usage{Windows: []provider.Window{{Kind: provider.Weekly, UsedPercent: 40, ResetsAt: now.Add(2 * time.Hour)}}}, nil
		})},
		{Provider: provider.Claude, Name: "stale", Source: fetchFunc(func(context.Context) (provider.Usage, error) {
			return provider.Usage{}, errors.New("claude token endpoint returned HTTP 400; the slot was not changed, run 'hop login claude <account>': claude token refresh failed")
		})},
	}
	var output bytes.Buffer
	if err := showAccountsFrom(context.Background(), &output, false, catalog, now); err != nil {
		t.Fatalf("showAccountsFrom() error = %v", err)
	}
	want := "CLAUDE\n" +
		"    ACCOUNT   HEADROOM                      RESET\n" +
		"  + work    ############........    60%   2h00m\n" +
		"  ! stale                         ERROR\n" +
		"\n" +
		"attention\n" +
		" 1. claude/stale: usage unavailable (HTTP 400); hop login claude stale\n" +
		"\n" +
		"+ 50-100 plenty   ~ 10-49 tight   o 0-9 nearly/full   ! error   > active\n"
	if got := output.String(); got != want {
		t.Fatalf("showAccountsFrom() table mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}
