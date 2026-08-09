// Package codex reads Codex credentials, refreshes slot tokens, and fetches usage.
package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/janiorvalle/hop/internal/provider"
)

const (
	defaultUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	defaultTokenURL = "https://auth.openai.com/oauth/token"
	// The public client ID published in the open-source Codex CLI. It is not a
	// secret and not issued to hop; OpenAI can change it at any time, which is
	// why Config.ClientID can override it.
	defaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	fiveHourSeconds = 18_000
	weeklySeconds   = 604_800
	responseLimit   = 1 << 20
)

var (
	ErrCredentials = errors.New("codex credentials are invalid")
	ErrRefresh     = errors.New("codex token refresh failed")
	ErrUsage       = errors.New("codex usage request failed")
)

// Credentials is the OAuth token set stored in auth.json.
type Credentials struct {
	AuthMode     string `json:"-"`
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	LastRefresh  string `json:"-"`
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
	TokenURL   string
	ClientID   string
	Now        func() time.Time
}

// Adapter talks to Codex OAuth and usage endpoints.
type Adapter struct {
	client   *http.Client
	usageURL string
	tokenURL string
	clientID string
	now      func() time.Time
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
	return Adapter{client: client, usageURL: usageURL, tokenURL: tokenURL, clientID: clientID, now: now}
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
	if strings.TrimSpace(credentials.AccessToken) == "" || strings.TrimSpace(credentials.AccountID) == "" {
		return provider.Usage{}, fmt.Errorf("access token or account ID is missing; run 'hop login codex <account>': %w", ErrCredentials)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, adapter.usageURL, nil)
	if err != nil {
		return provider.Usage{}, fmt.Errorf("build Codex usage request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	request.Header.Set("chatgpt-account-id", credentials.AccountID)

	response, err := adapter.client.Do(request)
	if err != nil {
		return provider.Usage{}, fmt.Errorf("reach Codex usage endpoint; check the network and retry: %w: %w", err, ErrUsage)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return provider.Usage{}, fmt.Errorf("codex usage returned HTTP %d; run 'hop login codex <account>' and retry: %w", response.StatusCode, ErrUsage)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit))
	if err != nil {
		return provider.Usage{}, fmt.Errorf("read Codex usage response; retry the command: %w: %w", err, ErrUsage)
	}
	return parseUsage(body, adapter.now())
}

// Refresh rotates OAuth tokens in a hop-owned store. Never pass the live auth.json store.
func (adapter Adapter) Refresh(ctx context.Context, store Store) (Credentials, error) {
	credentials, err := store.Read()
	if err != nil {
		return Credentials{}, err
	}
	if strings.TrimSpace(credentials.RefreshToken) == "" {
		return Credentials{}, fmt.Errorf("refresh token is missing; run 'hop login codex <account>': %w", ErrCredentials)
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {credentials.RefreshToken},
		"client_id":     {adapter.clientID},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, fmt.Errorf("build Codex refresh request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := adapter.client.Do(request)
	if err != nil {
		return Credentials{}, fmt.Errorf("reach Codex token endpoint; the slot was not changed and the request can be retried: %w: %w", err, ErrRefresh)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Credentials{}, fmt.Errorf("codex token endpoint returned HTTP %d; the slot was not changed, run 'hop login codex <account>': %w", response.StatusCode, ErrRefresh)
	}

	var refreshed refreshResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, responseLimit))
	if err := decoder.Decode(&refreshed); err != nil {
		return Credentials{}, fmt.Errorf("decode Codex refresh response; the slot was not changed and the request can be retried: %w: %w", err, ErrRefresh)
	}
	if refreshed.AccessToken == "" {
		return Credentials{}, fmt.Errorf("codex refresh response omitted access_token; the slot was not changed, run 'hop login codex <account>': %w", ErrRefresh)
	}

	credentials.AccessToken = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		credentials.RefreshToken = refreshed.RefreshToken
	}
	if refreshed.IDToken != "" {
		credentials.IDToken = refreshed.IDToken
	}
	credentials.LastRefresh = adapter.now().UTC().Format(time.RFC3339Nano)
	if err := store.Write(credentials); err != nil {
		return credentials, fmt.Errorf("save rotated Codex tokens; the returned credentials are the recovery copy and must be saved before retrying: %w: %w", err, ErrRefresh)
	}
	return credentials, nil
}

type refreshResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// NeedsRefresh reports whether the access token is a JWT that expires within skew.
// Opaque tokens are left alone so a read-only glance can still try them safely.
func (credentials Credentials) NeedsRefresh(now time.Time, skew time.Duration) bool {
	parts := strings.Split(credentials.AccessToken, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 {
		return false
	}
	return time.Unix(claims.ExpiresAt, 0).Before(now.Add(skew))
}

type usageResponse struct {
	PlanType             string                `json:"plan_type"`
	Email                string                `json:"email"`
	RateLimit            rateLimit             `json:"rate_limit"`
	AdditionalRateLimits []additionalRateLimit `json:"additional_rate_limits"`
}

type rateLimit struct {
	PrimaryWindow        *usageWindow          `json:"primary_window"`
	SecondaryWindow      *usageWindow          `json:"secondary_window"`
	AdditionalRateLimits []additionalRateLimit `json:"additional_rate_limits"`
}

type usageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type additionalRateLimit struct {
	LimitName      string    `json:"limit_name"`
	MeteredFeature string    `json:"metered_feature"`
	RateLimit      rateLimit `json:"rate_limit"`
}

func parseUsage(body []byte, now time.Time) (provider.Usage, error) {
	var response usageResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return provider.Usage{}, fmt.Errorf("decode Codex usage response; update hop because the provider response changed: %w: %w", err, ErrUsage)
	}
	usage := provider.Usage{
		Provider: provider.Codex,
		Email:    response.Email,
		Plan:     response.PlanType,
		Windows:  make([]provider.Window, 0, 2),
		Limits:   make([]provider.Limit, 0),
	}
	baseByKind := make(map[provider.WindowKind]float64)
	for _, window := range []*usageWindow{response.RateLimit.PrimaryWindow, response.RateLimit.SecondaryWindow} {
		if window == nil {
			continue
		}
		normalized, err := normalizeWindow(*window, now)
		if err != nil {
			return provider.Usage{}, err
		}
		usage.Windows = append(usage.Windows, normalized)
		baseByKind[normalized.Kind] = normalized.UsedPercent
	}

	additional := append(response.AdditionalRateLimits, response.RateLimit.AdditionalRateLimits...)
	for _, meter := range additional {
		scope := meter.LimitName
		if scope == "" {
			scope = meter.MeteredFeature
		}
		if scope == "" {
			return provider.Usage{}, fmt.Errorf("decode Codex usage response; an additional rate limit omitted its name, update hop before retrying: %w", ErrUsage)
		}
		for _, window := range []*usageWindow{meter.RateLimit.PrimaryWindow, meter.RateLimit.SecondaryWindow} {
			if window == nil {
				continue
			}
			normalized, err := normalizeWindow(*window, now)
			if err != nil {
				return provider.Usage{}, err
			}
			usage.Limits = append(usage.Limits, provider.Limit{
				Kind:        "model_" + string(normalized.Kind),
				Group:       "model",
				UsedPercent: normalized.UsedPercent,
				Severity:    severity(normalized.UsedPercent),
				ResetsAt:    normalized.ResetsAt,
				Scope:       scope,
				Active:      normalized.UsedPercent >= baseByKind[normalized.Kind],
			})
		}
	}
	if len(usage.Windows) == 0 && len(usage.Limits) == 0 {
		return provider.Usage{}, fmt.Errorf("decode Codex usage response; no usage windows or limits were present, update hop before retrying: %w", ErrUsage)
	}
	return usage, nil
}

func normalizeWindow(window usageWindow, now time.Time) (provider.Window, error) {
	var kind provider.WindowKind
	switch window.LimitWindowSeconds {
	case fiveHourSeconds:
		kind = provider.FiveHour
	case weeklySeconds:
		kind = provider.Weekly
	default:
		return provider.Window{}, fmt.Errorf("classify Codex %d-second usage window; update hop with the provider's new window duration: %w", window.LimitWindowSeconds, ErrUsage)
	}
	var resetsAt time.Time
	if window.ResetAt > 0 {
		resetsAt = time.Unix(window.ResetAt, 0).UTC()
	} else if window.ResetAfterSeconds > 0 {
		resetsAt = now.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
	} else {
		return provider.Window{}, fmt.Errorf("decode Codex %s window; reset_at and reset_after_seconds are missing, update hop before retrying: %w", kind, ErrUsage)
	}
	return provider.Window{Kind: kind, UsedPercent: window.UsedPercent, ResetsAt: resetsAt}, nil
}

func severity(percent float64) string {
	switch {
	case percent >= 90:
		return "critical"
	case percent >= 70:
		return "warning"
	default:
		return "normal"
	}
}
