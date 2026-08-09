// Package claude reads Claude credentials, refreshes slot tokens, and fetches usage.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

const (
	defaultUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	defaultProfileURL = "https://api.anthropic.com/api/oauth/profile"
	defaultTokenURL   = "https://platform.claude.com/v1/oauth/token"
	// The public Claude Code OAuth client ID that community tooling uses. It
	// is not a secret and not issued to hop; Anthropic can change it at any
	// time, which is why Config.ClientID can override it.
	defaultClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	betaHeaderValue = "oauth-2025-04-20"
	responseLimit   = 1 << 20
)

var (
	ErrCredentials = errors.New("claude credentials are invalid")
	ErrProfile     = errors.New("claude profile request failed")
	ErrRefresh     = errors.New("claude token refresh failed")
	ErrUsage       = errors.New("claude usage request failed")
)

// Credentials is the OAuth object stored below .claudeAiOauth.
type Credentials struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken"`
	ExpiresAt             int64    `json:"expiresAt"`
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt"`
	SubscriptionType      string   `json:"subscriptionType,omitempty"`
	RateLimitTier         string   `json:"rateLimitTier,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
}

// Profile identifies the Claude account that owns an OAuth access token.
type Profile struct {
	AccountUUID string
	Email       string
}

// Store reads and writes credentials in a hop-owned account slot.
type Store interface {
	Read() (Credentials, error)
	Write(Credentials) error
}

// Config overrides adapter dependencies and endpoints for tests.
type Config struct {
	HTTPClient *http.Client
	UsageURL   string
	ProfileURL string
	TokenURL   string
	ClientID   string
	Now        func() time.Time
}

// Adapter talks to the Claude OAuth and usage endpoints.
type Adapter struct {
	client     *http.Client
	usageURL   string
	profileURL string
	tokenURL   string
	clientID   string
	now        func() time.Time
}

type credentialFetcher struct {
	adapter     Adapter
	credentials Credentials
}

var _ provider.Fetcher = credentialFetcher{}

// New returns an adapter with production defaults unless Config overrides them.
func New(config Config) Adapter {
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	usageURL := config.UsageURL
	if usageURL == "" {
		usageURL = defaultUsageURL
	}
	profileURL := config.ProfileURL
	if profileURL == "" {
		profileURL = defaultProfileURL
	}
	tokenURL := config.TokenURL
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	clientID := config.ClientID
	if clientID == "" {
		clientID = defaultClientID
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return Adapter{client: client, usageURL: usageURL, profileURL: profileURL, tokenURL: tokenURL, clientID: clientID, now: now}
}

// Fetcher binds credentials to the shared account fetcher contract.
func (adapter Adapter) Fetcher(credentials Credentials) provider.Fetcher {
	return credentialFetcher{adapter: adapter, credentials: credentials}
}

func (fetcher credentialFetcher) FetchUsage(ctx context.Context) (provider.Usage, error) {
	return fetcher.adapter.FetchUsage(ctx, fetcher.credentials)
}

// FetchUsage returns normalized limits without modifying credentials.
func (adapter Adapter) FetchUsage(ctx context.Context, credentials Credentials) (provider.Usage, error) {
	if strings.TrimSpace(credentials.AccessToken) == "" {
		return provider.Usage{}, fmt.Errorf("access token is missing; run 'hop login claude <account>': %w", ErrCredentials)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, adapter.usageURL, nil)
	if err != nil {
		return provider.Usage{}, fmt.Errorf("build Claude usage request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	request.Header.Set("anthropic-beta", betaHeaderValue)

	response, err := adapter.client.Do(request)
	if err != nil {
		return provider.Usage{}, fmt.Errorf("reach Claude usage endpoint; check the network and retry: %w: %w", err, ErrUsage)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return provider.Usage{}, fmt.Errorf("claude usage returned HTTP %d; %s: %w", response.StatusCode, provider.UsageHTTPAction(provider.Claude, response.StatusCode), ErrUsage)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit))
	if err != nil {
		return provider.Usage{}, fmt.Errorf("read Claude usage response; retry the command: %w: %w", err, ErrUsage)
	}
	usage, err := parseUsage(body)
	if err != nil {
		return provider.Usage{}, err
	}
	return usage, nil
}

// FetchProfile returns the account that owns the supplied live access token.
func (adapter Adapter) FetchProfile(ctx context.Context, credentials Credentials) (Profile, error) {
	if strings.TrimSpace(credentials.AccessToken) == "" {
		return Profile{}, fmt.Errorf("access token is missing; run 'hop login claude <account>': %w", ErrCredentials)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, adapter.profileURL, nil)
	if err != nil {
		return Profile{}, fmt.Errorf("build Claude profile request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	request.Header.Set("anthropic-beta", betaHeaderValue)

	response, err := adapter.client.Do(request)
	if err != nil {
		return Profile{}, fmt.Errorf("reach Claude profile endpoint; check the network and retry: %w: %w", err, ErrProfile)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Profile{}, fmt.Errorf("claude profile returned HTTP %d; refresh the live Claude login and retry: %w", response.StatusCode, ErrProfile)
	}

	var responseProfile profileResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, responseLimit))
	if err := decoder.Decode(&responseProfile); err != nil {
		return Profile{}, fmt.Errorf("decode Claude profile response; retry the command: %w: %w", err, ErrProfile)
	}
	profile := Profile{
		AccountUUID: strings.TrimSpace(responseProfile.Account.UUID),
		Email:       strings.TrimSpace(responseProfile.Account.Email),
	}
	if profile.AccountUUID == "" || profile.Email == "" {
		return Profile{}, fmt.Errorf("claude profile omitted account.uuid or account.email; retry with a refreshed Claude login: %w", ErrProfile)
	}
	return profile, nil
}

// Refresh rotates OAuth tokens in a hop-owned store. Never pass a live credential store.
func (adapter Adapter) Refresh(ctx context.Context, store Store) (Credentials, error) {
	credentials, err := store.Read()
	if err != nil {
		return Credentials{}, err
	}
	if strings.TrimSpace(credentials.RefreshToken) == "" {
		return Credentials{}, fmt.Errorf("refresh token is missing; run 'hop login claude <account>': %w", ErrCredentials)
	}

	payload, err := json.Marshal(refreshRequest{
		GrantType:    "refresh_token",
		RefreshToken: credentials.RefreshToken,
		ClientID:     adapter.clientID,
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("encode Claude refresh request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.tokenURL, bytes.NewReader(payload))
	if err != nil {
		return Credentials{}, fmt.Errorf("build Claude refresh request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-beta", betaHeaderValue)

	response, err := adapter.client.Do(request)
	if err != nil {
		return Credentials{}, fmt.Errorf("reach Claude token endpoint; the slot was not changed and the request can be retried: %w: %w", err, ErrRefresh)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Credentials{}, fmt.Errorf("claude token endpoint returned HTTP %d; the slot was not changed, run 'hop login claude <account>': %w", response.StatusCode, ErrRefresh)
	}

	var refreshed refreshResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, responseLimit))
	if err := decoder.Decode(&refreshed); err != nil {
		return Credentials{}, fmt.Errorf("decode Claude refresh response; the slot was not changed and the request can be retried: %w: %w", err, ErrRefresh)
	}
	if refreshed.AccessToken == "" || refreshed.RefreshToken == "" || refreshed.ExpiresIn <= 0 {
		return Credentials{}, fmt.Errorf("claude refresh response omitted required tokens or expiry; the slot was not changed, run 'hop login claude <account>': %w", ErrRefresh)
	}

	credentials.AccessToken = refreshed.AccessToken
	credentials.RefreshToken = refreshed.RefreshToken
	credentials.ExpiresAt = adapter.now().Add(time.Duration(refreshed.ExpiresIn) * time.Second).UnixMilli()
	if refreshed.RefreshTokenExpiresIn > 0 {
		credentials.RefreshTokenExpiresAt = adapter.now().Add(time.Duration(refreshed.RefreshTokenExpiresIn) * time.Second).UnixMilli()
	}
	if err := store.Write(credentials); err != nil {
		return credentials, fmt.Errorf("save rotated Claude tokens; the returned credentials are the recovery copy and must be saved before retrying: %w: %w", err, ErrRefresh)
	}
	return credentials, nil
}

// NeedsRefresh reports whether an access token expires within skew.
func (credentials Credentials) NeedsRefresh(now time.Time, skew time.Duration) bool {
	return credentials.ExpiresAt <= now.Add(skew).UnixMilli()
}

// HasScope reports whether Anthropic granted a named OAuth scope.
func (credentials Credentials) HasScope(scope string) bool {
	for _, granted := range credentials.Scopes {
		if strings.TrimSpace(granted) == scope {
			return true
		}
	}
	return false
}

type refreshRequest struct {
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
}

type refreshResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
}

type profileResponse struct {
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email"`
	} `json:"account"`
}

type usageResponse struct {
	FiveHour *usageWindow `json:"five_hour"`
	SevenDay *usageWindow `json:"seven_day"`
	Limits   []usageLimit `json:"limits"`
}

type usageWindow struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

type usageLimit struct {
	Kind     string          `json:"kind"`
	Group    string          `json:"group"`
	Percent  float64         `json:"percent"`
	Severity string          `json:"severity"`
	ResetsAt time.Time       `json:"resets_at"`
	Scope    json.RawMessage `json:"scope"`
	Active   bool            `json:"is_active"`
}

func parseUsage(body []byte) (provider.Usage, error) {
	var response usageResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return provider.Usage{}, fmt.Errorf("decode Claude usage response; update hop because the provider response changed: %w: %w", err, ErrUsage)
	}
	usage := provider.Usage{
		Provider: provider.Claude,
		Windows:  make([]provider.Window, 0, 2),
		Limits:   make([]provider.Limit, 0, len(response.Limits)),
	}
	if response.FiveHour != nil {
		if response.FiveHour.ResetsAt.IsZero() && response.FiveHour.Utilization != 0 {
			return provider.Usage{}, fmt.Errorf("decode Claude five-hour usage window; resets_at may be null only while utilization is zero, update hop before retrying: %w", ErrUsage)
		}
		usage.Windows = append(usage.Windows, provider.Window{Kind: provider.FiveHour, UsedPercent: response.FiveHour.Utilization, ResetsAt: response.FiveHour.ResetsAt})
	}
	if response.SevenDay != nil {
		if response.SevenDay.ResetsAt.IsZero() && response.SevenDay.Utilization != 0 {
			return provider.Usage{}, fmt.Errorf("decode Claude seven-day usage window; resets_at may be null only while utilization is zero, update hop before retrying: %w", ErrUsage)
		}
		usage.Windows = append(usage.Windows, provider.Window{Kind: provider.Weekly, UsedPercent: response.SevenDay.Utilization, ResetsAt: response.SevenDay.ResetsAt})
	}
	for _, limit := range response.Limits {
		if limit.Kind == "" || limit.Group == "" {
			return provider.Usage{}, fmt.Errorf("decode Claude usage response; a limit omitted kind or group, update hop before retrying: %w", ErrUsage)
		}
		// An idle account reports its enforced limit as is_active with a null
		// resets_at because no usage window has ever started, so only consumed
		// percent demands a reset timestamp.
		if limit.ResetsAt.IsZero() && limit.Percent != 0 {
			return provider.Usage{}, fmt.Errorf("decode Claude %s limit; resets_at may be null only while percent is zero, update hop before retrying: %w", limit.Kind, ErrUsage)
		}
		usage.Limits = append(usage.Limits, provider.Limit{
			Kind:        limit.Kind,
			Group:       limit.Group,
			UsedPercent: limit.Percent,
			Severity:    limit.Severity,
			ResetsAt:    limit.ResetsAt,
			Scope:       parseScope(limit.Scope),
			Active:      limit.Active,
		})
	}
	if len(usage.Windows) == 0 && len(usage.Limits) == 0 {
		return provider.Usage{}, fmt.Errorf("decode Claude usage response; no usage windows or limits were present, update hop before retrying: %w", ErrUsage)
	}
	return usage, nil
}

func parseScope(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var scope string
	if err := json.Unmarshal(raw, &scope); err == nil {
		return scope
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return string(raw)
}
