package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewStoreDoesNotExposeUndoConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	_, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "undo_enabled") || strings.Contains(string(b), "undo_window_minutes") {
		t.Fatal("undo controls must not remain configurable")
	}
}

func TestPollCountersAccumulateAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddPollResult(2, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPollResult(3, 0); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.Snapshot()
	if state.PollSucceeded != 5 || state.PollFailed != 1 {
		t.Fatalf("unexpected persisted poll counters: succeeded=%d failed=%d", state.PollSucceeded, state.PollFailed)
	}
}

func TestVersion2MigrationRebaselinesAndRemovesOldUndoConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
  "version": 2,
  "config": {
    "poll_interval_seconds": 60,
    "confirm_delay_seconds": 10,
    "natural_grace_seconds": 120,
    "max_sample_age_seconds": 300,
    "drop_epsilon_percent": 0.2,
    "bark_server_url": "https://api.day.app",
    "bark_group": "Sub2API 自动重置",
    "bark_level": "active",
    "undo_enabled": true,
    "undo_window_minutes": 15,
    "sources": []
  },
  "accounts": {"1": {"initialized": true, "last_used_percent": 42}},
  "events": []
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().Version != stateVersion {
		t.Fatalf("version = %d, want %d", store.Snapshot().Version, stateVersion)
	}
	account, ok := store.Account(1)
	if !ok || account.Initialized {
		t.Fatalf("migration must invalidate the old baseline: %+v", account)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "drop_epsilon_percent") {
		t.Fatal("deprecated percentage threshold remained in migrated state")
	}
	if strings.Contains(string(b), "undo_enabled") || strings.Contains(string(b), "undo_window_minutes") {
		t.Fatal("deprecated undo controls remained in migrated state")
	}
}

func TestVersion4MigrationSetsSuccessfulEventUndoToOneDay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
  "version": 4,
  "config": {
    "poll_interval_seconds": 60,
    "confirm_delay_seconds": 10,
    "natural_grace_seconds": 120,
    "max_sample_age_seconds": 300,
    "bark_server_url": "https://api.day.app",
    "bark_group": "Sub2API 自动重置",
    "bark_level": "active",
    "undo_enabled": false,
    "undo_window_minutes": 15,
    "sources": []
  },
  "accounts": {},
  "events": [{
    "id": "event-1",
    "detected_at": "2026-08-30T00:00:00Z",
    "undo_expires_at": "2026-08-30T00:15:00Z",
    "targets": [{"subscription_id": 9, "status": "succeeded"}]
  }]
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	events := store.Snapshot().Events
	if len(events) != 1 || events[0].UndoExpiresAt != "2026-08-31T00:00:00Z" {
		t.Fatalf("unexpected migrated undo expiry: %+v", events)
	}
}

func TestVersion3MigrationRebaselinesForNativeWindowStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
  "version": 3,
  "config": {
    "poll_interval_seconds": 60,
    "confirm_delay_seconds": 10,
    "natural_grace_seconds": 120,
    "max_sample_age_seconds": 300,
    "bark_server_url": "https://api.day.app",
    "bark_group": "Sub2API 自动重置",
    "bark_level": "active",
    "undo_enabled": true,
    "undo_window_minutes": 1440,
    "sources": []
  },
  "accounts": {"1": {"initialized": true, "last_used_percent": 42}},
  "events": []
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	account, ok := store.Account(1)
	if !ok || account.Initialized || account.LastDecision != "native_usage_upgrade_rebaseline" {
		t.Fatalf("migration must establish a fresh native-usage baseline: %+v", account)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "last_used_percent") {
		t.Fatal("legacy percentage baseline remained in migrated state")
	}
}

func TestVersion5MigrationSeedsLatestConfirmedResetFuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
  "version": 5,
  "config": {"poll_interval_seconds": 60, "confirm_delay_seconds": 10, "natural_grace_seconds": 120, "max_sample_age_seconds": 300, "sources": []},
  "accounts": {"1": {"initialized": true, "expected_reset_at": 1788653047}},
  "events": [
    {"id":"new", "source_account_id":1, "detected_at":"2026-08-30T22:29:33Z", "confirmed_reset_at":1788653047, "targets":[{"subscription_id":3,"status":"succeeded"}]},
    {"id":"old", "source_account_id":1, "detected_at":"2026-08-30T17:21:11Z", "confirmed_reset_at":1788653048, "targets":[{"subscription_id":3,"status":"succeeded"}]}
  ]
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	account, ok := store.Account(1)
	if !ok || account.LastConfirmedResetAt != 1788653047 || account.LastConfirmedAt != "2026-08-30T22:29:33Z" {
		t.Fatalf("latest successful event was not migrated: %+v", account)
	}
}

func TestEventReturnsRequestedRecordCopy(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := ResetEvent{ID: "event-one", Targets: []TargetResult{{SubscriptionID: 1, Status: "succeeded"}}}
	second := ResetEvent{ID: "event-two", Targets: []TargetResult{{SubscriptionID: 2, Status: "succeeded"}}}
	if err := store.AddEvent(first); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEvent(second); err != nil {
		t.Fatal(err)
	}

	event, ok := store.Event(first.ID)
	if !ok || event.ID != first.ID || len(event.Targets) != 1 || event.Targets[0].SubscriptionID != 1 {
		t.Fatalf("unexpected event lookup: %+v", event)
	}
	event.Targets[0].Status = "changed"
	stored, ok := store.Event(first.ID)
	if !ok || stored.Targets[0].Status != "succeeded" {
		t.Fatalf("event lookup exposed mutable store state: %+v", stored)
	}
	if _, ok := store.Event("missing"); ok {
		t.Fatal("missing event unexpectedly found")
	}
}
