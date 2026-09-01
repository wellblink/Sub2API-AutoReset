package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientListsOnlyParentOAuthAccounts(t *testing.T) {
	parentID := int64(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "admin-test" {
			t.Fatal("missing admin API key")
		}
		if r.URL.Query().Get("status") != "" {
			t.Fatal("OAuth listing must filter status locally")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "success", "data": map[string]any{
				"items": []any{
					map[string]any{"id": 1, "name": "parent", "platform": "openai", "type": "oauth", "status": "active", "quota_dimension": "global", "group_ids": []int64{10}, "account_groups": []any{map[string]any{"group_id": 11}}},
					map[string]any{"id": 2, "name": "shadow", "platform": "openai", "type": "oauth", "status": "active", "parent_account_id": parentID, "quota_dimension": "spark"},
					map[string]any{"id": 3, "name": "inactive", "platform": "openai", "type": "oauth", "status": "disabled", "quota_dimension": "global"},
				},
				"total": 2, "page": 1, "page_size": 1000, "pages": 1,
			},
		})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := client.ListOAuthAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != 1 {
		t.Fatalf("unexpected accounts: %+v", accounts)
	}
	groups := accounts[0].mappingGroupIDs()
	if _, ok := groups[10]; !ok {
		t.Fatalf("group_ids relationship was not decoded: %+v", accounts[0])
	}
	if _, ok := groups[11]; !ok {
		t.Fatalf("account_groups relationship was not decoded: %+v", accounts[0])
	}
}

func TestClientResetSubscriptionUsesOfficialPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/subscriptions/42/reset-quota" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]bool
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !body["daily"] || !body["weekly"] || body["monthly"] {
			t.Fatalf("unexpected body: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": map[string]any{"id": 42}})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ResetSubscription(context.Background(), 42, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != 42 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestPersistedWeeklyMetadataReusesFreshSnapshotWithoutUpstreamQuery(t *testing.T) {
	var paths []string
	fetchedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	resetAt := fetchedAt.Add(6 * 24 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Path != "/admin/accounts/7" {
			t.Fatalf("unexpected request %s", r.URL.RequestURI())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": map[string]any{
			"id":          7,
			"credentials": map[string]any{"chatgpt_account_id": "upstream-7", "plan_type": "pro"},
			"extra": map[string]any{
				"codex_usage_updated_at":  fetchedAt.Format(time.RFC3339),
				"codex_7d_reset_at":       resetAt.Format(time.RFC3339),
				"codex_7d_window_minutes": 10080,
				"codex_reset_credit_snapshot": map[string]any{"available_count": 2, "credits": []any{
					map[string]any{"expires_at": resetAt.Add(8 * 24 * time.Hour).Format(time.RFC3339)},
					map[string]any{"expires_at": resetAt.Add(9 * 24 * time.Hour).Format(time.RFC3339)},
				}},
			},
		}})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := client.PersistedWeeklyMetadata(context.Background(), 7, 5*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/admin/accounts/7" {
		t.Fatalf("fresh snapshot must only read the local account API: %+v", paths)
	}
	if s.ForcedRefresh || s.FetchedAt != fetchedAt.Unix() || s.ResetAt != resetAt.Unix() || s.WindowSeconds != codexWeeklyWindowSeconds || s.UpstreamAccountID != "upstream-7" || s.PlanType != "pro" || s.CreditCount != 2 || !s.CreditCountKnown || !s.CreditDetailsComplete || len(s.CreditExpirations) != 2 {
		t.Fatalf("unexpected sample: %+v", s)
	}
}

func TestPersistedWeeklyMetadataForcesStaleSnapshotAndWaitsForPersistence(t *testing.T) {
	var paths []string
	oldFetchedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	newFetchedAt := time.Now().UTC().Truncate(time.Second)
	resetAt := newFetchedAt.Add(7 * 24 * time.Hour)
	accountReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/admin/accounts/7":
			accountReads++
			fetchedAt := oldFetchedAt
			if accountReads > 1 {
				fetchedAt = newFetchedAt
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": map[string]any{
				"id":          7,
				"credentials": map[string]any{"chatgpt_account_id": "upstream-7", "plan_type": "pro"},
				"extra": map[string]any{
					"codex_usage_updated_at":      fetchedAt.Format(time.RFC3339),
					"codex_7d_reset_at":           resetAt.Format(time.RFC3339),
					"codex_7d_window_minutes":     10080,
					"codex_reset_credit_snapshot": map[string]any{"available_count": 0},
				},
			}})
		case "/admin/accounts/7/usage":
			if r.URL.Query().Get("source") != "active" || r.URL.Query().Get("force") != "true" {
				t.Fatalf("stale snapshot was not forcibly refreshed: %s", r.URL.RequestURI())
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "success", "data": map[string]any{}})
		default:
			t.Fatalf("unexpected request %s", r.URL.RequestURI())
		}
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := client.PersistedWeeklyMetadata(context.Background(), 7, 5*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || paths[0] != "/admin/accounts/7" || paths[1] != "/admin/accounts/7/usage?source=active&force=true" || paths[2] != "/admin/accounts/7" {
		t.Fatalf("unexpected refresh sequence: %+v", paths)
	}
	if !s.ForcedRefresh || s.FetchedAt != newFetchedAt.Unix() || s.CreditCount != 0 || !s.CreditCountKnown || !s.CreditDetailsComplete {
		t.Fatalf("unexpected refreshed sample: %+v", s)
	}
}

func TestValidateAdminBearerDoesNotUseServiceAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/auth/me" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer browser-admin-token" {
			t.Fatalf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-API-Key") != "" {
			t.Fatal("browser validation must not inherit the sidecar admin API key")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "success", "data": map[string]any{"id": 1, "role": "admin"},
		})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ValidateAdminBearer(context.Background(), "Bearer browser-admin-token"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAdminBearerRejectsRegularUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "success", "data": map[string]any{"id": 2, "role": "user"},
		})
	}))
	defer server.Close()
	client, err := NewAPIClient(server.URL, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ValidateAdminBearer(context.Background(), "Bearer regular-user-token"); err == nil {
		t.Fatal("regular user bearer token must be rejected")
	}
}

func TestValidateConfigDeduplicatesTargets(t *testing.T) {
	cfg := defaultConfig()
	cfg.Sources = []SourceConfig{{
		AccountID: 9, Enabled: true, ResetWeekly: true,
		TargetSubscriptionIDs: []int64{3, 2, 3},
	}}
	if err := validateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	got := cfg.Sources[0].TargetSubscriptionIDs
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("targets were not normalized: %+v", got)
	}
}

func TestReconcileSourceMappingsRemovesCrossGroupTargets(t *testing.T) {
	cfg := defaultConfig()
	cfg.Sources = []SourceConfig{{
		AccountID: 1, AccountName: "stale name", Enabled: true, ResetWeekly: true,
		TargetSubscriptionIDs: []int64{101, 102, 103, 404},
	}}
	accounts := []Account{{
		ID: 1, Name: "OAuth 主账号", GroupIDs: []int64{10},
		AccountGroups: []AccountGroupSummary{{GroupID: 11}},
	}}
	subscriptions := []Subscription{
		{ID: 101, GroupID: 10, Status: "active"},
		{ID: 102, GroupID: 11, Status: "active"},
		{ID: 103, GroupID: 12, Status: "active"},
	}

	result, removed, err := reconcileSourceMappings(cfg, accounts, subscriptions)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("expected two incompatible targets removed, got %d", removed)
	}
	got := result.Sources[0]
	if got.AccountName != "OAuth 主账号" || len(got.TargetSubscriptionIDs) != 2 || got.TargetSubscriptionIDs[0] != 101 || got.TargetSubscriptionIDs[1] != 102 {
		t.Fatalf("unexpected reconciled source: %+v", got)
	}
	if len(cfg.Sources[0].TargetSubscriptionIDs) != 4 {
		t.Fatal("reconciliation mutated the caller's target list")
	}
}

func TestReconcileSourceMappingsRejectsUnknownOAuthSource(t *testing.T) {
	cfg := defaultConfig()
	cfg.Sources = []SourceConfig{{AccountID: 99, Enabled: true, ResetWeekly: true}}
	if _, _, err := reconcileSourceMappings(cfg, nil, nil); err == nil {
		t.Fatal("unknown OAuth source must be rejected")
	}
}
