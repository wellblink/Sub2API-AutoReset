package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	codexWeeklyWindowSeconds      = int64(7 * 24 * 60 * 60)
	persistedUsageWaitTimeout     = 3 * time.Second
	persistedUsageWaitPoll        = 100 * time.Millisecond
	persistedUsageFutureTolerance = time.Minute
)

type ResetCreditDetail struct {
	ExpiresAt string `json:"expires_at"`
}

type pageData[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Pages    int   `json:"pages"`
}

type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type APIClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewAPIClient(baseURL, apiKey string) (*APIClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || apiKey == "" {
		return nil, errors.New("base URL and admin API key are required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("invalid Sub2API base URL")
	}
	return &APIClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http: &http.Client{
			Timeout: 35 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          20,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}, nil
}

func (c *APIClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("User-Agent", "sub2api-quota-sync/1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var env apiEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("Sub2API returned HTTP %d with invalid JSON", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || env.Code != 0 {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("Sub2API HTTP %d: %s", resp.StatusCode, msg)
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode Sub2API data: %w", err)
		}
	}
	return nil
}

// ValidateAdminBearer verifies that a browser session belongs to a Sub2API
// administrator. The session token is never replaced by the sidecar's admin API
// key, so a regular user's token cannot inherit sidecar privileges.
func (c *APIClient) ValidateAdminBearer(ctx context.Context, authorization string) error {
	authorization = strings.TrimSpace(authorization)
	if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) > 16<<10 {
		return errors.New("invalid bearer authorization")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/auth/me", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", authorization)
	req.Header.Set("X-Admin-UI-Request", "1")
	req.Header.Set("X-User-UI-Request", "1")
	req.Header.Set("User-Agent", "sub2api-quota-sync/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var env apiEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return errors.New("invalid Sub2API authentication response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || env.Code != 0 {
		return errors.New("Sub2API administrator session rejected")
	}
	var profile struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(env.Data, &profile); err != nil || profile.Role != "admin" {
		return errors.New("Sub2API session does not belong to an administrator")
	}
	return nil
}

func (c *APIClient) ListOAuthAccounts(ctx context.Context) ([]Account, error) {
	var page pageData[Account]
	// Sub2API currently has a server-side status filter inconsistency that can
	// omit an account even when the returned account status itself is active.
	// Fetch the OAuth set first and apply the status predicate locally.
	path := "/admin/accounts?page=1&page_size=1000&platform=openai&type=oauth&lite=true&sort_by=name&sort_order=asc"
	if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	items := make([]Account, 0, len(page.Items))
	for _, a := range page.Items {
		if a.Status == "active" && a.ParentAccountID == nil && (a.QuotaDimension == "" || a.QuotaDimension == "global") {
			items = append(items, a)
		}
	}
	return items, nil
}

func (c *APIClient) ListActiveSubscriptions(ctx context.Context) ([]Subscription, error) {
	items := []Subscription{}
	for pageNum := 1; pageNum <= 1000; pageNum++ {
		var page pageData[Subscription]
		path := "/admin/subscriptions?page=" + strconv.Itoa(pageNum) + "&page_size=1000&status=active&sort_by=created_at&sort_order=desc"
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.Pages <= pageNum || len(page.Items) == 0 {
			break
		}
	}
	return items, nil
}

// ForceAccountUsage calls the endpoint behind Account Management's “查询”
// action and explicitly requests a new upstream observation. Normal cached
// reads never enter this method.
func (c *APIClient) ForceAccountUsage(ctx context.Context, accountID int64) error {
	path := "/admin/accounts/" + strconv.FormatInt(accountID, 10) + "/usage?source=active&force=true"
	return c.do(ctx, http.MethodGet, path, nil, nil)
}

// PersistedWeeklyMetadata reads the timestamped Codex window and reset-credit
// cache from the account record. A normal poll reuses a snapshot younger than
// reuseFor without touching an upstream endpoint. A stale/missing snapshot, or
// forceRefresh=true for candidate confirmation, performs the official forced
// usage query and waits until Sub2API's asynchronous account-extra write is
// visible. The quota endpoint is intentionally absent from this path because it
// always calls OpenAI even though the reset-credit button already persists the
// data we need in codex_reset_credit_snapshot.
func (c *APIClient) PersistedWeeklyMetadata(ctx context.Context, accountID int64, reuseFor time.Duration, forceRefresh bool) (WeeklySample, error) {
	account, err := c.GetAccount(ctx, accountID)
	if err != nil {
		return WeeklySample{}, fmt.Errorf("read persisted account snapshot: %w", err)
	}
	previousFetchedAt, timestampErr := codexUsageFetchedAt(account.Extra)
	shouldForce := forceRefresh || timestampErr != nil || !usageSnapshotFresh(previousFetchedAt, reuseFor, time.Now())
	if shouldForce {
		if err := c.ForceAccountUsage(ctx, accountID); err != nil {
			return WeeklySample{}, fmt.Errorf("force active account usage query: %w", err)
		}
		account, err = c.waitForPersistedUsageSnapshot(ctx, accountID, previousFetchedAt)
		if err != nil {
			return WeeklySample{}, err
		}
	}
	sample, err := extractPersistedWeekly(account, time.Now())
	if err != nil {
		return WeeklySample{}, err
	}
	sample.ForcedRefresh = shouldForce
	return sample, nil
}

func (c *APIClient) waitForPersistedUsageSnapshot(ctx context.Context, accountID int64, previous time.Time) (Account, error) {
	deadline := time.Now().Add(persistedUsageWaitTimeout)
	var lastErr error
	for {
		account, err := c.GetAccount(ctx, accountID)
		if err != nil {
			lastErr = err
		} else if fetchedAt, parseErr := codexUsageFetchedAt(account.Extra); parseErr != nil {
			lastErr = parseErr
		} else if previous.IsZero() || fetchedAt.After(previous) {
			return account, nil
		} else {
			lastErr = fmt.Errorf("codex_usage_updated_at did not advance from %s", previous.Format(time.RFC3339))
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Account{}, fmt.Errorf("forced usage query was not persisted by Sub2API within %s: %w", persistedUsageWaitTimeout, lastErr)
		}
		wait := persistedUsageWaitPoll
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Account{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *APIClient) GetSubscription(ctx context.Context, id int64) (Subscription, error) {
	var result Subscription
	path := "/admin/subscriptions/" + strconv.FormatInt(id, 10)
	err := c.do(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (c *APIClient) GetAccount(ctx context.Context, id int64) (Account, error) {
	var result Account
	path := "/admin/accounts/" + strconv.FormatInt(id, 10)
	err := c.do(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (c *APIClient) ResetSubscription(ctx context.Context, id int64, daily, weekly, monthly bool) (Subscription, error) {
	var result Subscription
	path := "/admin/subscriptions/" + strconv.FormatInt(id, 10) + "/reset-quota"
	body := map[string]bool{"daily": daily, "weekly": weekly, "monthly": monthly}
	err := c.do(ctx, http.MethodPost, path, body, &result)
	return result, err
}

func extractPersistedWeekly(account Account, now time.Time) (WeeklySample, error) {
	fetchedAt, err := codexUsageFetchedAt(account.Extra)
	if err != nil {
		return WeeklySample{}, err
	}
	resetAt, err := codexWeeklyResetAt(account.Extra, fetchedAt)
	if err != nil {
		return WeeklySample{}, err
	}
	windowSeconds, err := codexWeeklyWindow(account.Extra)
	if err != nil {
		return WeeklySample{}, err
	}
	sample := WeeklySample{
		FetchedAt:             fetchedAt.Unix(),
		ResetAt:               resetAt.Unix(),
		WindowSeconds:         windowSeconds,
		UpstreamAccountID:     stringValue(account.Credentials["chatgpt_account_id"]),
		PlanType:              stringValue(account.Credentials["plan_type"]),
		CreditCountKnown:      false,
		CreditDetailsComplete: false,
	}
	applyPersistedResetCredits(&sample, account.Extra, now)
	return sample, nil
}

func codexUsageFetchedAt(extra map[string]any) (time.Time, error) {
	raw, ok := extra["codex_usage_updated_at"]
	if !ok {
		return time.Time{}, errors.New("Sub2API account has no persisted codex_usage_updated_at")
	}
	parsed, err := parsePersistedTime(raw)
	if err != nil || parsed.IsZero() {
		return time.Time{}, errors.New("Sub2API account has invalid codex_usage_updated_at")
	}
	return parsed, nil
}

func codexWeeklyResetAt(extra map[string]any, fetchedAt time.Time) (time.Time, error) {
	if raw, ok := extra["codex_7d_reset_at"]; ok {
		if parsed, err := parsePersistedTime(raw); err == nil && !parsed.IsZero() {
			return parsed, nil
		}
	}
	if seconds, ok := int64Value(extra["codex_7d_reset_after_seconds"]); ok && seconds >= 0 && !fetchedAt.IsZero() {
		return fetchedAt.Add(time.Duration(seconds) * time.Second), nil
	}
	return time.Time{}, errors.New("Sub2API account has no persisted 7d natural reset time")
}

func codexWeeklyWindow(extra map[string]any) (int64, error) {
	raw, ok := extra["codex_7d_window_minutes"]
	if !ok {
		return codexWeeklyWindowSeconds, nil
	}
	minutes, ok := int64Value(raw)
	if !ok || minutes < 24*60 || minutes > 31*24*60 {
		return 0, errors.New("Sub2API account has invalid persisted 7d window length")
	}
	return minutes * 60, nil
}

func usageSnapshotFresh(sampledAt time.Time, reuseFor time.Duration, now time.Time) bool {
	if sampledAt.IsZero() || reuseFor <= 0 {
		return false
	}
	age := now.Sub(sampledAt)
	return age >= -persistedUsageFutureTolerance && age < reuseFor
}

func applyPersistedResetCredits(sample *WeeklySample, extra map[string]any, now time.Time) {
	if sample == nil || extra == nil {
		return
	}
	raw, ok := extra["codex_reset_credit_snapshot"]
	if !ok || raw == nil {
		return
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return
	}
	var cached struct {
		AvailableCount *int                `json:"available_count"`
		Credits        []ResetCreditDetail `json:"credits"`
	}
	if err := json.Unmarshal(encoded, &cached); err != nil || cached.AvailableCount == nil || *cached.AvailableCount < 0 {
		return
	}
	if *cached.AvailableCount == 0 {
		sample.CreditCountKnown = true
		sample.CreditDetailsComplete = true
		return
	}

	expirations := make([]string, 0, len(cached.Credits))
	complete := true
	for _, credit := range cached.Credits {
		expiresAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(credit.ExpiresAt))
		if parseErr != nil {
			complete = false
			continue
		}
		if !expiresAt.After(now) {
			continue
		}
		expirations = append(expirations, expiresAt.UTC().Format(time.RFC3339Nano))
	}
	available := *cached.AvailableCount
	if len(expirations) < available {
		available = len(expirations)
	}
	// Match the account page: once a positive cached count has no usable card
	// details left, the value is unknown and a reset candidate must fail closed.
	if available <= 0 {
		return
	}
	sort.Strings(expirations)
	sample.CreditCount = available
	sample.CreditCountKnown = true
	sample.CreditExpirations = expirations
	sample.CreditDetailsComplete = complete && len(expirations) == available
}

func parsePersistedTime(value any) (time.Time, error) {
	raw := strings.TrimSpace(stringValue(value))
	if raw == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	return time.Parse(time.RFC3339Nano, raw)
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
