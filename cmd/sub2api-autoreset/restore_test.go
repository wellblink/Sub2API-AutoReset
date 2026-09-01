package main

import (
	"math"
	"testing"
	"time"
)

func TestResolveRestoreWindowsPreservesSameWindowConsumption(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	beforeStart := now.Add(-24 * time.Hour)
	afterStart := now.Add(-time.Minute)
	before := SubscriptionUsageSnapshot{WeeklyWindowStart: &beforeStart}
	after := SubscriptionUsageSnapshot{WeeklyWindowStart: &afterStart}
	current := currentSubscriptionUsage{WeeklyWindowStart: &afterStart, WeeklyUsageUSD: 2.5}
	plan, err := resolveRestoreWindows(current, before, after, false, true, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Weekly || plan.Daily || plan.Monthly {
		t.Fatalf("unexpected restore plan: %+v", plan)
	}
}

func TestResolveRestoreWindowsRejectsLaterResetInsideOriginalPeriod(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	beforeStart := now.Add(-24 * time.Hour)
	afterStart := now.Add(-time.Minute)
	laterStart := now
	before := SubscriptionUsageSnapshot{WeeklyWindowStart: &beforeStart}
	after := SubscriptionUsageSnapshot{WeeklyWindowStart: &afterStart}
	current := currentSubscriptionUsage{WeeklyWindowStart: &laterStart}
	if _, err := resolveRestoreWindows(current, before, after, false, true, false, now); err == nil {
		t.Fatal("expected later window reset to block rollback")
	}
}

func TestResolveRestoreWindowsRestoresDailyInsideSameCalendarDay(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.Local)
	dayStart := time.Date(2026, 8, 30, 0, 0, 0, 0, time.Local)
	before := SubscriptionUsageSnapshot{DailyWindowStart: &dayStart}
	after := SubscriptionUsageSnapshot{DailyWindowStart: &dayStart}
	current := currentSubscriptionUsage{DailyWindowStart: &dayStart}
	plan, err := resolveRestoreWindows(current, before, after, true, false, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Daily {
		t.Fatalf("same-day daily usage should be restored: %+v", plan)
	}
}

func TestResolveRestoreWindowsSkipsExpiredDailyButKeepsWeeklyAndMonthly(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, zone)
	yesterdayStart := time.Date(2026, 8, 30, 0, 0, 0, 0, zone)
	todayStart := time.Date(2026, 8, 31, 0, 0, 0, 0, zone)
	beforeWeeklyStart := time.Date(2026, 8, 27, 9, 0, 0, 0, zone)
	beforeMonthlyStart := time.Date(2026, 8, 10, 9, 0, 0, 0, zone)
	resetAt := time.Date(2026, 8, 30, 20, 0, 0, 0, zone)
	before := SubscriptionUsageSnapshot{
		DailyWindowStart:   &yesterdayStart,
		WeeklyWindowStart:  &beforeWeeklyStart,
		MonthlyWindowStart: &beforeMonthlyStart,
	}
	after := SubscriptionUsageSnapshot{
		DailyWindowStart:   &yesterdayStart,
		WeeklyWindowStart:  &resetAt,
		MonthlyWindowStart: &resetAt,
	}
	current := currentSubscriptionUsage{
		DailyWindowStart:   &todayStart,
		WeeklyWindowStart:  &resetAt,
		MonthlyWindowStart: &resetAt,
	}

	plan, err := resolveRestoreWindows(current, before, after, true, true, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Daily || !plan.Weekly || !plan.Monthly {
		t.Fatalf("expired daily usage must stay out of today's window: %+v", plan)
	}
}

func TestSubscriptionSnapshotKeepsDownstreamIdentity(t *testing.T) {
	sub := Subscription{
		ID: 3, UserID: 18, GroupID: 7,
		User:  &UserSummary{ID: 18, Email: "user@example.com", Username: "example-user"},
		Group: &GroupSummary{ID: 7, Name: "默认分组", Platform: "openai"},
	}
	snapshot := snapshotSubscription(sub, time.Now())
	if snapshot.UserEmail != "user@example.com" || snapshot.Username != "example-user" || snapshot.GroupName != "默认分组" {
		t.Fatalf("downstream identity was not preserved: %+v", snapshot)
	}
}

func TestBuildUndoTargetPreviewAddsPostResetUsage(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	beforeStart := now.Add(-24 * time.Hour)
	afterStart := now.Add(-time.Hour)
	event := ResetEvent{ResetWeekly: true}
	target := TargetResult{
		SubscriptionID: 3,
		BeforeReset: &SubscriptionUsageSnapshot{
			SubscriptionID:    3,
			UserID:            18,
			GroupID:           7,
			WeeklyUsageUSD:    58.33,
			WeeklyWindowStart: &beforeStart,
		},
		AfterReset: &SubscriptionUsageSnapshot{
			SubscriptionID:    3,
			UserID:            18,
			GroupID:           7,
			WeeklyWindowStart: &afterStart,
		},
	}
	current := Subscription{
		ID:                3,
		UserID:            18,
		GroupID:           7,
		WeeklyUsageUSD:    2.65,
		WeeklyWindowStart: &afterStart,
	}
	preview, err := buildUndoTargetPreview(event, target, current, now)
	if err != nil {
		t.Fatal(err)
	}
	if preview.BeforeReset.WeeklyUsageUSD != 58.33 || preview.AfterReset.WeeklyUsageUSD != 2.65 || preview.AfterUndo.WeeklyUsageUSD != 60.98 {
		t.Fatalf("unexpected undo preview: %+v", preview)
	}
}

func TestBuildUndoTargetPreviewDoesNotMoveYesterdayDailyUsageIntoToday(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, zone)
	yesterdayStart := time.Date(2026, 8, 30, 0, 0, 0, 0, zone)
	todayStart := time.Date(2026, 8, 31, 0, 0, 0, 0, zone)
	beforeWeeklyStart := time.Date(2026, 8, 27, 9, 0, 0, 0, zone)
	beforeMonthlyStart := time.Date(2026, 8, 10, 9, 0, 0, 0, zone)
	resetAt := time.Date(2026, 8, 30, 20, 0, 0, 0, zone)
	event := ResetEvent{ResetDaily: true, ResetWeekly: true, ResetMonthly: true}
	target := TargetResult{
		SubscriptionID: 3,
		BeforeReset: &SubscriptionUsageSnapshot{
			SubscriptionID: 3, UserID: 18, GroupID: 7,
			DailyUsageUSD: 2.65, WeeklyUsageUSD: 58.33, MonthlyUsageUSD: 100,
			DailyWindowStart: &yesterdayStart, WeeklyWindowStart: &beforeWeeklyStart, MonthlyWindowStart: &beforeMonthlyStart,
		},
		AfterReset: &SubscriptionUsageSnapshot{
			SubscriptionID: 3, UserID: 18, GroupID: 7,
			DailyWindowStart: &yesterdayStart, WeeklyWindowStart: &resetAt, MonthlyWindowStart: &resetAt,
		},
	}
	current := Subscription{
		ID: 3, UserID: 18, GroupID: 7,
		DailyUsageUSD: 0.41, WeeklyUsageUSD: 0.41, MonthlyUsageUSD: 0.41,
		DailyWindowStart: &todayStart, WeeklyWindowStart: &resetAt, MonthlyWindowStart: &resetAt,
	}

	preview, err := buildUndoTargetPreview(event, target, current, now)
	if err != nil {
		t.Fatal(err)
	}
	if !almostEqual(preview.AfterUndo.DailyUsageUSD, 0.41) ||
		!almostEqual(preview.AfterUndo.WeeklyUsageUSD, 58.74) ||
		!almostEqual(preview.AfterUndo.MonthlyUsageUSD, 100.41) {
		t.Fatalf("unexpected cross-day undo preview: %+v", preview)
	}
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.000001
}
