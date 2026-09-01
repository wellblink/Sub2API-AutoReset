package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stubWindowTotalsReader struct {
	totals UsageTotals
	called bool
	id     int64
	start  time.Time
}

type stubUsageRestorer struct {
	calledTargets []int64
	result        RestoreResult
	err           error
}

func (s *stubUsageRestorer) Restore(_ context.Context, target TargetResult, _, _, _ bool, _ time.Time) (RestoreResult, error) {
	s.calledTargets = append(s.calledTargets, target.SubscriptionID)
	return s.result, s.err
}

func (s *stubWindowTotalsReader) ReadAccountWindowTotals(_ context.Context, accountID int64, start time.Time) (UsageTotals, error) {
	s.called = true
	s.id = accountID
	s.start = start
	return s.totals, nil
}

func baseState() AccountState {
	return AccountState{
		Initialized: true, LastUsage: UsageTotals{Requests: 170, Tokens: 14_800_000, Cost: 7.32, StandardCost: 7.32, UserCost: 7.32}, LastFetchedAt: 1_000,
		ExpectedResetAt: 10_000, LastCreditCount: 3, CreditCountKnown: true, CreditDetailsComplete: true,
		LastCreditExpirations: []string{"2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z", "2026-09-03T00:00:00Z"},
		UpstreamAccountID:     "upstream-a", PlanType: "pro", WindowSeconds: 604800,
	}
}

func sample(usage UsageTotals) WeeklySample {
	return WeeklySample{
		Usage: usage, FetchedAt: 1_060, ResetAt: 10_000,
		WindowSeconds: 604800, CreditCount: 3, CreditCountKnown: true, CreditDetailsComplete: true,
		CreditExpirations: []string{"2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z", "2026-09-03T00:00:00Z"},
		UpstreamAccountID: "upstream-a", PlanType: "pro",
	}
}

func droppedSample() WeeklySample {
	return sample(UsageTotals{Requests: 8, Tokens: 650_000, Cost: 0.41, StandardCost: 0.41, UserCost: 0.41})
}

func testConfig() Config {
	c := defaultConfig()
	c.NaturalGraceSeconds = 120
	return c
}

func TestClassifyManualResetWithNonzeroUsage(t *testing.T) {
	if got := classify(baseState(), droppedSample(), testConfig()); got != DecisionCandidate {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyNaturalReset(t *testing.T) {
	p := baseState()
	p.ExpectedResetAt = 1_100
	if got := classify(p, droppedSample(), testConfig()); got != DecisionNaturalReset {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyConsumedResetCredit(t *testing.T) {
	s := droppedSample()
	s.CreditCount = 2
	if got := classify(baseState(), s, testConfig()); got != DecisionCreditDecrease {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyMissingCreditDataFailsClosed(t *testing.T) {
	s := droppedSample()
	s.CreditCountKnown = false
	if got := classify(baseState(), s, testConfig()); got != DecisionCreditUnknown {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyMissingCreditExpiryDetailsFailsClosed(t *testing.T) {
	s := droppedSample()
	s.CreditDetailsComplete = false
	if got := classify(baseState(), s, testConfig()); got != DecisionCreditUnknown {
		t.Fatalf("got %s", got)
	}
}

func TestClassifySameCreditCountWithReplacementFailsClosed(t *testing.T) {
	s := droppedSample()
	s.CreditExpirations = []string{"2026-09-02T00:00:00Z", "2026-09-03T00:00:00Z", "2026-09-04T00:00:00Z"}
	if got := classify(baseState(), s, testConfig()); got != DecisionCreditChanged {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyPureCreditAdditionDoesNotHideOfficialReset(t *testing.T) {
	s := droppedSample()
	s.CreditCount = 4
	s.CreditExpirations = append(s.CreditExpirations, "2026-09-04T00:00:00Z")
	if got := classify(baseState(), s, testConfig()); got != DecisionCandidate {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyRequestDropTriggersEvenWhenCustomersConsumedAfterReset(t *testing.T) {
	s := sample(UsageTotals{Requests: 169, Tokens: 15_200_000, Cost: 7.80, StandardCost: 7.80, UserCost: 7.80})
	if got := classify(baseState(), s, testConfig()); got != DecisionCandidate {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyUnchangedUsageDoesNotTrigger(t *testing.T) {
	if got := classify(baseState(), sample(baseState().LastUsage), testConfig()); got != DecisionSteady {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyGrowingNativeUsageDoesNotTrigger(t *testing.T) {
	s := sample(UsageTotals{Requests: 180, Tokens: 16_000_000, Cost: 8.10, StandardCost: 8.10, UserCost: 8.10})
	if got := classify(baseState(), s, testConfig()); got != DecisionSteady {
		t.Fatalf("got %s", got)
	}
}

func TestClassifyMonetaryDropTriggersWhenExactCountersCaughtUp(t *testing.T) {
	s := sample(UsageTotals{Requests: 175, Tokens: 15_000_000, Cost: 6.90, StandardCost: 6.90, UserCost: 6.90})
	if got := classify(baseState(), s, testConfig()); got != DecisionCandidate {
		t.Fatalf("got %s", got)
	}
}

func TestClassifySuppressesRepeatedConfirmedWindow(t *testing.T) {
	p := baseState()
	p.LastConfirmedResetAt = p.ExpectedResetAt
	if got := classify(p, droppedSample(), testConfig()); got != DecisionDuplicateReset {
		t.Fatalf("got %s", got)
	}

	s := droppedSample()
	s.ResetAt = p.ExpectedResetAt + int64(nativeWindowDriftTolerance/time.Second) + 1
	if got := classify(p, s, testConfig()); got != DecisionCandidate {
		t.Fatalf("materially new reset deadline got %s", got)
	}
}

func TestStabilizeNativeWindowUsesPreviousExactBoundary(t *testing.T) {
	const resetAt int64 = 1_788_653_046
	p := baseState()
	p.ExpectedResetAt = resetAt
	p.WindowSeconds = 7 * 24 * 60 * 60
	current := droppedSample()
	current.ResetAt = resetAt + 2
	current.WindowSeconds = p.WindowSeconds
	stable := UsageTotals{Requests: 2959, Tokens: 380_409_217, Cost: 257.66904869, StandardCost: 257.66904869, UserCost: 257.66904869}
	reader := &stubWindowTotalsReader{totals: stable}
	engine := &Engine{window: reader}

	got, err := engine.stabilizeNativeWindow(context.Background(), 1, p, current)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.called || reader.id != 1 || reader.start.Unix() != resetAt-p.WindowSeconds {
		t.Fatalf("unexpected fixed-window query: called=%v id=%d start=%s", reader.called, reader.id, reader.start)
	}
	if got.ResetAt != resetAt || got.Usage != stable {
		t.Fatalf("sample was not normalized: %+v", got)
	}
	if decision := classify(p, got, testConfig()); decision != DecisionSteady {
		t.Fatalf("boundary-only decrease remained actionable: %s", decision)
	}
}

func TestStabilizeNativeWindowLeavesMaterialResetChangeUntouched(t *testing.T) {
	const resetAt int64 = 1_788_653_046
	p := baseState()
	p.ExpectedResetAt = resetAt
	p.WindowSeconds = 7 * 24 * 60 * 60
	current := droppedSample()
	current.ResetAt = resetAt + int64(nativeWindowDriftTolerance/time.Second) + 1
	current.WindowSeconds = p.WindowSeconds
	reader := &stubWindowTotalsReader{}
	engine := &Engine{window: reader}

	got, err := engine.stabilizeNativeWindow(context.Background(), 1, p, current)
	if err != nil {
		t.Fatal(err)
	}
	if reader.called || got.ResetAt != current.ResetAt || got.Usage != current.Usage {
		t.Fatalf("material reset was incorrectly normalized: %+v", got)
	}
}

func TestClassifyRequiresPriorNaturalDeadline(t *testing.T) {
	p := baseState()
	p.ExpectedResetAt = 0
	if got := classify(p, droppedSample(), testConfig()); got != DecisionNoDeadline {
		t.Fatalf("got %s", got)
	}
}

func TestValidateSampleAge(t *testing.T) {
	now := time.Unix(1_000, 0)
	if err := validateSampleAge(WeeklySample{FetchedAt: 700}, 299, now); err == nil {
		t.Fatal("expected stale sample error")
	}
}

func TestExtractPersistedWeeklyUsesAccountMetadata(t *testing.T) {
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	fetchedAt := now.Add(-time.Minute)
	resetAt := now.Add(6 * 24 * time.Hour)
	account := Account{
		Credentials: map[string]any{"chatgpt_account_id": "a", "plan_type": "pro"},
		Extra: map[string]any{
			"codex_usage_updated_at":  fetchedAt.Format(time.RFC3339),
			"codex_7d_reset_at":       resetAt.Format(time.RFC3339),
			"codex_7d_window_minutes": 10080,
			"codex_reset_credit_snapshot": map[string]any{
				"available_count": 4,
				"credits": []any{
					map[string]any{"expires_at": "2026-09-01T00:00:00Z"},
					map[string]any{"expires_at": "2026-09-02T00:00:00Z"},
					map[string]any{"expires_at": "2026-09-03T00:00:00Z"},
					map[string]any{"expires_at": "2026-09-04T00:00:00Z"},
				},
			},
		},
	}
	s, err := extractPersistedWeekly(account, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.FetchedAt != fetchedAt.Unix() || s.ResetAt != resetAt.Unix() || s.UpstreamAccountID != "a" || s.PlanType != "pro" || s.WindowSeconds != 604800 || s.CreditCount != 4 || !s.CreditCountKnown || !s.CreditDetailsComplete {
		t.Fatalf("unexpected sample: %+v", s)
	}
}

func TestExtractPersistedWeeklyFiltersExpiredCreditsAndFailsClosedWithoutCache(t *testing.T) {
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	extra := map[string]any{
		"codex_usage_updated_at":  now.Add(-time.Minute).Format(time.RFC3339),
		"codex_7d_reset_at":       now.Add(6 * 24 * time.Hour).Format(time.RFC3339),
		"codex_7d_window_minutes": 10080,
	}
	unknown, err := extractPersistedWeekly(Account{Extra: extra}, now)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.CreditCountKnown || unknown.CreditDetailsComplete {
		t.Fatalf("missing cache must stay unknown: %+v", unknown)
	}

	extra["codex_reset_credit_snapshot"] = map[string]any{
		"available_count": 2,
		"credits": []any{
			map[string]any{"expires_at": now.Add(-time.Hour).Format(time.RFC3339)},
			map[string]any{"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339)},
		},
	}
	filtered, err := extractPersistedWeekly(Account{Extra: extra}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !filtered.CreditCountKnown || !filtered.CreditDetailsComplete || filtered.CreditCount != 1 || len(filtered.CreditExpirations) != 1 {
		t.Fatalf("expired credits were not filtered like the S2A card: %+v", filtered)
	}
}

func TestResolveActionSourceHonorsConfigChangedDuringConfirmation(t *testing.T) {
	startCfg := testConfig()
	startCfg.Enabled = true
	start := SourceConfig{AccountID: 7, Enabled: true, TargetSubscriptionIDs: []int64{10, 11}, ResetDaily: true, ResetWeekly: true}
	startCfg.Sources = []SourceConfig{start}

	latest := startCfg
	latest.Sources = []SourceConfig{{AccountID: 7, Enabled: true, TargetSubscriptionIDs: []int64{11, 12}, ResetWeekly: true}}
	resolved, allowed, reason := resolveActionSource(startCfg, start, latest, true)
	if !allowed || reason != "" {
		t.Fatalf("unexpected suppression: allowed=%v reason=%s", allowed, reason)
	}
	if len(resolved.TargetSubscriptionIDs) != 1 || resolved.TargetSubscriptionIDs[0] != 11 {
		t.Fatalf("expected target intersection, got %+v", resolved.TargetSubscriptionIDs)
	}
	if resolved.ResetDaily || !resolved.ResetWeekly {
		t.Fatalf("expected dimension intersection, got %+v", resolved)
	}

	latest.Enabled = false
	_, allowed, reason = resolveActionSource(startCfg, start, latest, true)
	if allowed || reason != "config_disabled_or_mapping_removed" {
		t.Fatalf("disabled master switch did not suppress: allowed=%v reason=%s", allowed, reason)
	}
}

func TestFinalizePollSummaryReportsActualOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		input  PollSummary
		status string
		text   string
	}{
		{name: "normal", input: PollSummary{Sources: 2, Checked: 2}, status: "completed", text: "已检查 2 个账号，未检测到上游重置"},
		{name: "detected without action", input: PollSummary{Sources: 1, Checked: 1, Detected: 1}, status: "reset_detected", text: "检测到 1 个上游重置，未执行下游重置"},
		{name: "downstream action", input: PollSummary{Sources: 1, Checked: 1, Detected: 1, DownstreamSucceeded: 3, DownstreamFailed: 1}, status: "reset_performed", text: "下游重置 3 个成功，1 个失败"},
		{name: "all failed", input: PollSummary{Sources: 2, Failed: 2}, status: "failed", text: "2 个账号查询失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finalizePollSummary(&tt.input)
			if tt.input.Status != tt.status || !strings.Contains(tt.input.Message, tt.text) {
				t.Fatalf("unexpected summary: %+v", tt.input)
			}
			if strings.Contains(tt.input.Message, "基线") {
				t.Fatalf("manual result exposed internal baseline state: %s", tt.input.Message)
			}
		})
	}
}

func TestJitteredPollDelayKeepsConfiguredIntervalAsMinimum(t *testing.T) {
	interval := 120 * time.Second
	if got := jitteredPollDelay(interval, func(int64) int64 { return 0 }); got != interval {
		t.Fatalf("zero jitter delay = %s, want %s", got, interval)
	}
	got := jitteredPollDelay(interval, func(n int64) int64 { return n - 1 })
	if got < interval || got >= 150*time.Second {
		t.Fatalf("jittered delay = %s, want [120s, 150s)", got)
	}

	long := jitteredPollDelay(time.Hour, func(n int64) int64 { return n - 1 })
	if long < time.Hour || long >= time.Hour+automaticPollJitterMax {
		t.Fatalf("capped jittered delay = %s", long)
	}
}

func TestInitialPollOffsetIsBounded(t *testing.T) {
	got := initialPollOffset(120*time.Second, func(n int64) int64 { return n - 1 })
	if got < 0 || got >= 30*time.Second {
		t.Fatalf("initial offset = %s, want [0s, 30s)", got)
	}
}

func TestAutomaticSourceScheduleRandomizesOrderAndStaggersEveryAccount(t *testing.T) {
	sources := []SourceConfig{
		{AccountID: 1}, {AccountID: 2}, {AccountID: 3}, {AccountID: 4},
	}
	launches := sourceLaunchSchedule(sources, 120*time.Second, false, func(int64) int64 { return 0 })
	if len(launches) != len(sources) {
		t.Fatalf("launch count = %d, want %d", len(launches), len(sources))
	}
	if launches[0].Source.AccountID == sources[0].AccountID {
		t.Fatalf("source order was not shuffled: %+v", launches)
	}
	seen := make(map[int64]bool, len(launches))
	for i, launch := range launches {
		if seen[launch.Source.AccountID] {
			t.Fatalf("source %d scheduled twice", launch.Source.AccountID)
		}
		seen[launch.Source.AccountID] = true
		if i > 0 && launch.Delay <= launches[i-1].Delay {
			t.Fatalf("launches are not strictly staggered: %+v", launches)
		}
		if launch.Delay >= automaticSourceStaggerMax {
			t.Fatalf("automatic launch delay %s exceeds spread", launch.Delay)
		}
	}
}

func TestManualSourceScheduleAvoidsSimultaneousQueriesWithoutLongWait(t *testing.T) {
	sources := make([]SourceConfig, 20)
	for i := range sources {
		sources[i].AccountID = int64(i + 1)
	}
	launches := sourceLaunchSchedule(sources, 120*time.Second, true, func(int64) int64 { return 0 })
	for i, launch := range launches {
		if i > 0 && launch.Delay <= launches[i-1].Delay {
			t.Fatalf("manual launches are not strictly staggered: %+v", launches)
		}
		if launch.Delay >= manualSourceStaggerMax {
			t.Fatalf("manual launch delay %s is too long", launch.Delay)
		}
	}
}

func TestUndoTargetSelectsRequestedRecord(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	older := ResetEvent{
		ID:            "older-event",
		UndoExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		UndoStatus:    "available",
		Targets:       []TargetResult{{SubscriptionID: 10, Status: "succeeded"}},
	}
	newer := ResetEvent{
		ID:            "newer-event",
		UndoExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		UndoStatus:    "available",
		Targets:       []TargetResult{{SubscriptionID: 20, Status: "succeeded"}},
	}
	if err := store.AddEvent(older); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEvent(newer); err != nil {
		t.Fatal(err)
	}

	engine := &Engine{store: store, wake: make(chan struct{}, 1)}
	summary, err := engine.UndoTarget(context.Background(), older.ID, 10)
	if err == nil || !strings.Contains(err.Error(), "超过 24 小时") {
		t.Fatalf("expected requested older event to expire, got summary=%+v err=%v", summary, err)
	}
	if summary.EventID != older.ID {
		t.Fatalf("summary event ID = %q, want %q", summary.EventID, older.ID)
	}
	if summary.SubscriptionID != 10 {
		t.Fatalf("summary subscription ID = %d, want 10", summary.SubscriptionID)
	}
	olderState, ok := store.Event(older.ID)
	if !ok || olderState.UndoStatus != "expired" {
		t.Fatalf("requested event was not marked expired: %+v", olderState)
	}
	if olderState.Targets[0].UndoStatus != "expired" {
		t.Fatalf("requested user was not marked expired: %+v", olderState.Targets[0])
	}
	newerState, ok := store.Event(newer.ID)
	if !ok || newerState.UndoStatus != "available" {
		t.Fatalf("newer event was modified: %+v", newerState)
	}
}

func TestUndoTargetRestoresOnlyRequestedUser(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	firstSnapshot := &SubscriptionUsageSnapshot{SubscriptionID: 10, UserID: 101, GroupID: 1}
	secondSnapshot := &SubscriptionUsageSnapshot{SubscriptionID: 20, UserID: 202, GroupID: 1}
	event := ResetEvent{
		ID:            "multi-user-event",
		UndoExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		UndoStatus:    "available",
		ResetWeekly:   true,
		Targets: []TargetResult{
			{SubscriptionID: 10, Status: "succeeded", BeforeReset: firstSnapshot, AfterReset: firstSnapshot},
			{SubscriptionID: 20, Status: "succeeded", BeforeReset: secondSnapshot, AfterReset: secondSnapshot},
		},
	}
	if err := store.AddEvent(event); err != nil {
		t.Fatal(err)
	}
	restorer := &stubUsageRestorer{}
	engine := &Engine{store: store, restorer: restorer, wake: make(chan struct{}, 1)}
	summary, err := engine.UndoTarget(context.Background(), event.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SubscriptionID != 10 || summary.UserID != 101 || summary.Succeeded != 1 || summary.Failed != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if len(restorer.calledTargets) != 1 || restorer.calledTargets[0] != 10 {
		t.Fatalf("restore touched the wrong users: %+v", restorer.calledTargets)
	}
	updated, ok := store.Event(event.ID)
	if !ok {
		t.Fatal("event disappeared")
	}
	if updated.Targets[0].UndoStatus != "succeeded" {
		t.Fatalf("requested user was not marked succeeded: %+v", updated.Targets[0])
	}
	if updated.Targets[1].UndoStatus != "" || updated.Targets[1].UndoneAt != "" {
		t.Fatalf("other user was modified: %+v", updated.Targets[1])
	}
	if updated.UndoStatus != "partial" {
		t.Fatalf("event aggregate status = %q, want partial", updated.UndoStatus)
	}
}
