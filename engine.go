package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

const (
	automaticPollJitterDivisor  = 4
	automaticPollJitterMax      = 60 * time.Second
	automaticInitialOffsetMax   = 30 * time.Second
	automaticSourceStaggerMax   = 60 * time.Second
	manualSourceStaggerPerEntry = 250 * time.Millisecond
	manualSourceStaggerMax      = 2 * time.Second
	nativeWindowDriftTolerance  = 2 * time.Minute
)

type accountWindowTotalsReader interface {
	ReadAccountWindowTotals(context.Context, int64, time.Time) (UsageTotals, error)
}

type subscriptionUsageRestorer interface {
	Restore(context.Context, TargetResult, bool, bool, bool, time.Time) (RestoreResult, error)
}

type Engine struct {
	store    *Store
	client   *APIClient
	notifier *Notifier
	restorer subscriptionUsageRestorer
	window   accountWindowTotalsReader
	log      *slog.Logger

	pollMu sync.Mutex
	runMu  sync.RWMutex
	run    EngineRuntime
	wake   chan struct{}
	randN  func(int64) int64
}

func NewEngine(store *Store, client *APIClient, notifier *Notifier, restorer *UsageRestorer, logger *slog.Logger) *Engine {
	return &Engine{
		store: store, client: client, notifier: notifier, restorer: restorer, log: logger,
		window: restorer, wake: make(chan struct{}, 1), randN: rand.Int64N,
	}
}

func (e *Engine) Runtime() EngineRuntime {
	e.runMu.RLock()
	defer e.runMu.RUnlock()
	return e.run
}

func (e *Engine) NotifyConfigChanged() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) Run(ctx context.Context) {
	var next time.Time
	for {
		cfg := e.store.Config()
		if !cfg.Enabled {
			next = time.Time{}
			e.setNext(time.Time{})
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
				continue
			case <-time.After(time.Second):
				continue
			}
		}
		interval := time.Duration(cfg.PollIntervalSeconds) * time.Second
		if next.IsZero() {
			next = time.Now().Add(initialPollOffset(interval, e.randomInt64N))
		}
		e.setNext(next)
		wait := time.Until(next)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-e.wake:
				timer.Stop()
				next = time.Time{}
				continue
			case <-timer.C:
			}
		}
		e.PollAsync(ctx, false)
		next = time.Now().Add(jitteredPollDelay(interval, e.randomInt64N))
	}
}

type sourceLaunch struct {
	Source SourceConfig
	Delay  time.Duration
}

func (e *Engine) randomInt64N(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if e != nil && e.randN != nil {
		return e.randN(n)
	}
	return rand.Int64N(n)
}

func randomDurationBelow(max time.Duration, randomInt64N func(int64) int64) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(randomInt64N(int64(max)))
}

// The configured interval remains the minimum delay. A hidden positive jitter
// avoids a clock-like request cadence without ever increasing the configured
// upstream request rate. The cap keeps long polling intervals reasonably close
// to what the operator selected.
func jitteredPollDelay(interval time.Duration, randomInt64N func(int64) int64) time.Duration {
	if interval <= 0 {
		return 0
	}
	maxJitter := interval / automaticPollJitterDivisor
	if maxJitter > automaticPollJitterMax {
		maxJitter = automaticPollJitterMax
	}
	return interval + randomDurationBelow(maxJitter, randomInt64N)
}

// Restarts and configuration changes also receive a small random offset so a
// fleet restart cannot align all upstream probes on the same second.
func initialPollOffset(interval time.Duration, randomInt64N func(int64) int64) time.Duration {
	if interval <= 0 {
		return 0
	}
	maxOffset := interval / automaticPollJitterDivisor
	if maxOffset > automaticInitialOffsetMax {
		maxOffset = automaticInitialOffsetMax
	}
	return randomDurationBelow(maxOffset, randomInt64N)
}

// sourceLaunchSchedule shuffles account order every cycle and places each
// account in a separate time slot. Automatic runs spread across half of the
// nominal interval (capped at one minute); manual checks use only a short
// sub-two-second spread so the UI stays responsive. Slot-local randomness makes
// individual account timestamps irregular while keeping every launch distinct.
func sourceLaunchSchedule(sources []SourceConfig, interval time.Duration, manual bool, randomInt64N func(int64) int64) []sourceLaunch {
	launches := make([]sourceLaunch, len(sources))
	for i := range sources {
		launches[i].Source = sources[i]
	}
	for i := len(launches) - 1; i > 0; i-- {
		j := int(randomInt64N(int64(i + 1)))
		launches[i], launches[j] = launches[j], launches[i]
	}
	if len(launches) <= 1 {
		return launches
	}

	span := interval / 2
	if manual {
		span = time.Duration(len(launches)) * manualSourceStaggerPerEntry
		if span > manualSourceStaggerMax {
			span = manualSourceStaggerMax
		}
	} else if span > automaticSourceStaggerMax {
		span = automaticSourceStaggerMax
	}
	if span <= 0 {
		span = time.Duration(len(launches)) * time.Millisecond
	}
	slot := span / time.Duration(len(launches))
	if slot <= 0 {
		slot = time.Nanosecond
	}
	for i := range launches {
		launches[i].Delay = time.Duration(i)*slot + randomDurationBelow(slot/2, randomInt64N)
	}
	return launches
}

func waitUntil(ctx context.Context, deadline time.Time) bool {
	wait := time.Until(deadline)
	if wait <= 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (e *Engine) PollAsync(parent context.Context, force bool) bool {
	if !e.pollMu.TryLock() {
		return false
	}
	go func() {
		defer e.pollMu.Unlock()
		ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
		defer cancel()
		e.cycle(ctx, force)
	}()
	return true
}

func (e *Engine) PollNow(ctx context.Context) (PollSummary, error) {
	if !e.pollMu.TryLock() {
		return PollSummary{}, errors.New("检查、轮询或撤销任务正在运行")
	}
	defer e.pollMu.Unlock()
	return e.cycle(ctx, true), nil
}

type sourcePollResult struct {
	Checked             bool
	Detected            bool
	DownstreamSucceeded int
	DownstreamFailed    int
}

func (e *Engine) cycle(ctx context.Context, force bool) PollSummary {
	cfg := e.store.Config()
	summary := PollSummary{}
	started := time.Now().UTC()
	e.runMu.Lock()
	e.run.Running = true
	e.run.LastStartedAt = started.Format(time.RFC3339)
	e.run.LastError = ""
	e.runMu.Unlock()

	defer func() {
		if err := e.store.AddPollResult(summary.Checked, summary.Failed); err != nil {
			e.log.Error("persist poll counters failed", "error", err)
		}
		e.runMu.Lock()
		e.run.Running = false
		e.run.LastFinishedAt = time.Now().UTC().Format(time.RFC3339)
		result := summary
		e.run.LastResult = &result
		if summary.Failed > 0 {
			e.run.LastError = fmt.Sprintf("%d 个账号检查失败；请查看账号状态", summary.Failed)
		}
		e.runMu.Unlock()
	}()

	if !cfg.Enabled && !force {
		summary.Status = "disabled"
		summary.Message = "自动监听已关闭"
		return summary
	}
	sources := make([]SourceConfig, 0, len(cfg.Sources))
	for _, src := range cfg.Sources {
		if src.Enabled {
			sources = append(sources, src)
		}
	}
	if len(sources) == 0 {
		summary.Status = "no_sources"
		summary.Message = "没有可检查的上游账号，请先在账号映射中选择并启用 OAuth 账号"
		return summary
	}
	summary.Sources = len(sources)

	launches := sourceLaunchSchedule(
		sources,
		time.Duration(cfg.PollIntervalSeconds)*time.Second,
		force,
		e.randomInt64N,
	)
	launchEpoch := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	results := make(chan sourcePollResult, len(sources))
	errorsCh := make(chan error, len(sources))
	for i, launch := range launches {
		if !waitUntil(ctx, launchEpoch.Add(launch.Delay)) {
			for range launches[i:] {
				errorsCh <- ctx.Err()
			}
			break
		}
		source := launch.Source
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errorsCh <- ctx.Err()
				return
			}
			result, err := e.processSource(ctx, cfg, source, cfg.Enabled)
			if err != nil {
				e.log.Warn("source poll failed", "account_id", source.AccountID, "error", err)
				errorsCh <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for result := range results {
		if result.Checked {
			summary.Checked++
		}
		if result.Detected {
			summary.Detected++
		}
		summary.DownstreamSucceeded += result.DownstreamSucceeded
		summary.DownstreamFailed += result.DownstreamFailed
	}
	for range errorsCh {
		summary.Failed++
	}
	finalizePollSummary(&summary)
	return summary
}

func finalizePollSummary(summary *PollSummary) {
	switch {
	case summary.Detected > 0 && summary.DownstreamSucceeded+summary.DownstreamFailed > 0:
		summary.Status = "reset_performed"
		summary.Message = fmt.Sprintf("检查完成：检测到 %d 个上游重置；下游重置 %d 个成功，%d 个失败", summary.Detected, summary.DownstreamSucceeded, summary.DownstreamFailed)
	case summary.Detected > 0:
		summary.Status = "reset_detected"
		summary.Message = fmt.Sprintf("检查完成：检测到 %d 个上游重置，未执行下游重置", summary.Detected)
	case summary.Failed == summary.Sources && summary.Sources > 0:
		summary.Status = "failed"
		summary.Message = fmt.Sprintf("检查失败：%d 个账号查询失败", summary.Failed)
	case summary.Failed > 0:
		summary.Status = "partial"
		summary.Message = fmt.Sprintf("检查完成：%d 个账号正常，%d 个账号失败，未检测到上游重置", summary.Checked, summary.Failed)
	default:
		summary.Status = "completed"
		summary.Message = fmt.Sprintf("检查完成：已检查 %d 个账号，未检测到上游重置", summary.Checked)
	}
}

func (e *Engine) processSource(ctx context.Context, cfg Config, source SourceConfig, allowActions bool) (sourcePollResult, error) {
	result := sourcePollResult{}
	sample, err := e.readWeeklySample(ctx, cfg, source.AccountID, false)
	if err != nil {
		e.recordSourceError(source.AccountID, err)
		return result, err
	}
	if err := validateSampleAge(sample, cfg.MaxSampleAgeSeconds, time.Now()); err != nil {
		e.recordSourceError(source.AccountID, err)
		return result, err
	}
	prev, ok := e.store.Account(source.AccountID)
	if !ok {
		prev = AccountState{}
	}
	sample, err = e.stabilizeNativeWindow(ctx, source.AccountID, prev, sample)
	if err != nil {
		e.recordSourceError(source.AccountID, err)
		return result, err
	}
	decision := classify(prev, sample, cfg)
	if decision != DecisionCandidate {
		if decision != DecisionOutOfOrder {
			e.saveSample(source.AccountID, prev, sample, decision, "")
		}
		result.Checked = true
		return result, nil
	}

	e.log.Info("native weekly usage drop candidate", "account_id", source.AccountID, "previous", prev.LastUsage, "current", sample.Usage)
	timer := time.NewTimer(time.Duration(cfg.ConfirmDelaySeconds) * time.Second)
	select {
	case <-ctx.Done():
		timer.Stop()
		return result, ctx.Err()
	case <-timer.C:
	}
	// Confirmation must be a distinct upstream observation. Reusing the same
	// persisted timestamp would only prove that the local cache is unchanged.
	confirmed, err := e.readWeeklySample(ctx, cfg, source.AccountID, true)
	if err != nil {
		e.recordSourceError(source.AccountID, fmt.Errorf("confirmation query: %w", err))
		return result, err
	}
	if err := validateSampleAge(confirmed, cfg.MaxSampleAgeSeconds, time.Now()); err != nil {
		e.recordSourceError(source.AccountID, err)
		return result, err
	}
	confirmed, err = e.stabilizeNativeWindow(ctx, source.AccountID, prev, confirmed)
	if err != nil {
		e.recordSourceError(source.AccountID, err)
		return result, err
	}
	confirmDecision := classify(prev, confirmed, cfg)
	if confirmDecision != DecisionCandidate {
		if confirmDecision != DecisionOutOfOrder {
			e.saveSample(source.AccountID, prev, confirmed, confirmDecision, "")
		}
		result.Checked = true
		return result, nil
	}

	confirmedAt := time.Now().UTC()
	prev.Generation++
	prev.LastConfirmedResetAt = confirmed.ResetAt
	prev.LastConfirmedAt = confirmedAt.Format(time.RFC3339)
	e.saveSample(source.AccountID, prev, confirmed, Decision("manual_reset_confirmed"), "")
	latestCfg := e.store.Config()
	actionSource, actionAllowed, suppressedReason := resolveActionSource(cfg, source, latestCfg, allowActions)
	var sourceGroupIDs map[int64]struct{}
	if actionAllowed {
		currentSource, groupErr := e.client.GetAccount(ctx, source.AccountID)
		switch {
		case groupErr != nil:
			actionAllowed = false
			suppressedReason = "无法确认上游账号分组，已阻止下游重置"
			e.log.Error("source group safety check failed", "account_id", source.AccountID, "error", groupErr)
		case currentSource.Platform != "openai" || currentSource.Type != "oauth" || currentSource.Status != "active" || currentSource.ParentAccountID != nil || (currentSource.QuotaDimension != "" && currentSource.QuotaDimension != "global"):
			actionAllowed = false
			suppressedReason = "上游账号不再是可用的 OpenAI OAuth 父账号"
		default:
			sourceGroupIDs = currentSource.mappingGroupIDs()
			if len(sourceGroupIDs) == 0 {
				actionAllowed = false
				suppressedReason = "上游账号未关联任何分组"
			}
		}
	}
	event := ResetEvent{
		ID:                         fmt.Sprintf("%d-%d-%d", source.AccountID, confirmed.FetchedAt, prev.Generation),
		SourceAccountID:            source.AccountID,
		SourceAccountName:          source.AccountName,
		DetectedAt:                 confirmedAt.Format(time.RFC3339),
		PreviousUsage:              prev.LastUsage,
		CandidateUsage:             sample.Usage,
		ConfirmedUsage:             confirmed.Usage,
		PreviousResetAt:            prev.ExpectedResetAt,
		ConfirmedResetAt:           confirmed.ResetAt,
		PreviousCredits:            prev.LastCreditCount,
		ConfirmedCredits:           confirmed.CreditCount,
		PreviousCreditExpirations:  append([]string(nil), prev.LastCreditExpirations...),
		ConfirmedCreditExpirations: append([]string(nil), confirmed.CreditExpirations...),
		ResetDaily:                 actionSource.ResetDaily,
		ResetWeekly:                actionSource.ResetWeekly,
		ResetMonthly:               actionSource.ResetMonthly,
		SuppressedReason:           suppressedReason,
		Targets:                    make([]TargetResult, 0, len(actionSource.TargetSubscriptionIDs)),
	}
	if actionAllowed {
		event.UndoExpiresAt = time.Now().UTC().Add(undoWindow).Format(time.RFC3339)
		event.UndoStatus = "available"
	}
	for _, id := range actionSource.TargetSubscriptionIDs {
		status := "pending"
		if !actionAllowed {
			status = "skipped"
		}
		event.Targets = append(event.Targets, TargetResult{SubscriptionID: id, Status: status})
	}
	if err := e.store.AddEvent(event); err != nil {
		return result, fmt.Errorf("persist reset event: %w", err)
	}
	result.Checked = true
	result.Detected = true
	e.notifyDetection(event)
	if !actionAllowed || len(event.Targets) == 0 {
		e.log.Info("manual reset detected without downstream action", "account_id", source.AccountID, "reason", event.SuppressedReason, "targets", len(event.Targets))
		return result, nil
	}

	succeeded, failed := 0, 0
	for _, target := range event.Targets {
		if !e.targetStillAllowed(source.AccountID, target.SubscriptionID, event.ResetDaily, event.ResetWeekly, event.ResetMonthly) {
			_ = e.store.UpdateTarget(event.ID, target.SubscriptionID, "cancelled_config_changed", "执行前配置已关闭或映射已移除")
			failed++
			continue
		}
		before, err := e.client.GetSubscription(ctx, target.SubscriptionID)
		if err != nil {
			_ = e.store.CompleteTarget(event.ID, target.SubscriptionID, "failed", "保存重置前快照失败: "+err.Error(), nil)
			e.log.Error("capture downstream quota snapshot failed", "account_id", source.AccountID, "subscription_id", target.SubscriptionID, "error", err)
			failed++
			continue
		}
		if _, allowed := sourceGroupIDs[before.GroupID]; !allowed {
			_ = e.store.UpdateTarget(event.ID, target.SubscriptionID, "cancelled_group_changed", "订阅分组已不属于该 OAuth 账号")
			e.log.Warn("downstream reset blocked by group mapping", "account_id", source.AccountID, "subscription_id", target.SubscriptionID, "group_id", before.GroupID)
			failed++
			continue
		}
		beforeSnapshot := snapshotSubscription(before, time.Now())
		if err := e.store.PrepareTarget(event.ID, target.SubscriptionID, beforeSnapshot); err != nil {
			e.log.Error("persist downstream quota snapshot failed", "account_id", source.AccountID, "subscription_id", target.SubscriptionID, "error", err)
			failed++
			continue
		}
		after, err := e.client.ResetSubscription(ctx, target.SubscriptionID, event.ResetDaily, event.ResetWeekly, event.ResetMonthly)
		if err != nil {
			_ = e.store.CompleteTarget(event.ID, target.SubscriptionID, "failed", err.Error(), nil)
			e.log.Error("downstream quota reset failed", "account_id", source.AccountID, "subscription_id", target.SubscriptionID, "error", err)
			failed++
			continue
		}
		afterSnapshot := snapshotSubscription(after, time.Now())
		if err := e.store.CompleteTarget(event.ID, target.SubscriptionID, "succeeded", "", &afterSnapshot); err != nil {
			e.log.Error("persist downstream reset result failed", "account_id", source.AccountID, "subscription_id", target.SubscriptionID, "error", err)
			failed++
			continue
		}
		succeeded++
		e.log.Info("downstream quota reset succeeded", "account_id", source.AccountID, "subscription_id", target.SubscriptionID)
	}
	result.DownstreamSucceeded = succeeded
	result.DownstreamFailed = failed
	e.notifyReset(event, succeeded, failed)
	return result, nil
}

func (e *Engine) readWeeklySample(ctx context.Context, cfg Config, accountID int64, forceRefresh bool) (WeeklySample, error) {
	if e == nil || e.client == nil {
		return WeeklySample{}, errors.New("Sub2API 客户端未初始化")
	}
	if e.window == nil {
		return WeeklySample{}, errors.New("固定 7d 窗口读取器未初始化")
	}
	reuseSeconds := cfg.PollIntervalSeconds
	if cfg.MaxSampleAgeSeconds > 0 && (reuseSeconds <= 0 || cfg.MaxSampleAgeSeconds < reuseSeconds) {
		reuseSeconds = cfg.MaxSampleAgeSeconds
	}
	metadata, err := e.client.PersistedWeeklyMetadata(
		ctx,
		accountID,
		time.Duration(reuseSeconds)*time.Second,
		forceRefresh,
	)
	if err != nil {
		return WeeklySample{}, err
	}
	windowStart := metadata.ResetAt - metadata.WindowSeconds
	if windowStart <= 0 {
		return WeeklySample{}, errors.New("持久化 7d 窗口起点无效")
	}
	totals, err := e.window.ReadAccountWindowTotals(ctx, accountID, time.Unix(windowStart, 0))
	if err != nil {
		return WeeklySample{}, fmt.Errorf("读取 Sub2API 7d 累计用量: %w", err)
	}
	if totals.Requests < 0 || totals.Tokens < 0 || totals.Cost < 0 || totals.StandardCost < 0 || totals.UserCost < 0 {
		return WeeklySample{}, errors.New("Sub2API 7d 累计用量包含负数")
	}
	metadata.Usage = totals
	if e.log != nil {
		sampleSource := "persisted"
		if metadata.ForcedRefresh {
			sampleSource = "forced"
		}
		e.log.Info("weekly sample loaded",
			"account_id", accountID,
			"sample_source", sampleSource,
			"fetched_at", time.Unix(metadata.FetchedAt, 0).UTC().Format(time.RFC3339),
			"reset_at", time.Unix(metadata.ResetAt, 0).UTC().Format(time.RFC3339),
		)
	}
	return metadata, nil
}

func resolveActionSource(startCfg Config, start SourceConfig, latestCfg Config, allowActions bool) (SourceConfig, bool, string) {
	out := start
	out.TargetSubscriptionIDs = nil
	var latest SourceConfig
	found := false
	for _, src := range latestCfg.Sources {
		if src.AccountID == start.AccountID {
			latest = src
			found = true
			break
		}
	}
	if !found || !latest.Enabled || !start.Enabled || !allowActions || !startCfg.Enabled || !latestCfg.Enabled {
		return out, false, "config_disabled_or_mapping_removed"
	}
	out.AccountName = latest.AccountName
	latestTargets := make(map[int64]struct{}, len(latest.TargetSubscriptionIDs))
	for _, id := range latest.TargetSubscriptionIDs {
		latestTargets[id] = struct{}{}
	}
	for _, id := range start.TargetSubscriptionIDs {
		if _, ok := latestTargets[id]; ok {
			out.TargetSubscriptionIDs = append(out.TargetSubscriptionIDs, id)
		}
	}
	out.ResetDaily = start.ResetDaily && latest.ResetDaily
	out.ResetWeekly = start.ResetWeekly && latest.ResetWeekly
	out.ResetMonthly = start.ResetMonthly && latest.ResetMonthly
	if len(out.TargetSubscriptionIDs) == 0 {
		return out, false, "no_targets"
	}
	if !out.ResetDaily && !out.ResetWeekly && !out.ResetMonthly {
		return out, false, "no_reset_dimensions"
	}
	return out, true, ""
}

func (e *Engine) targetStillAllowed(accountID, targetID int64, daily, weekly, monthly bool) bool {
	cfg := e.store.Config()
	if !cfg.Enabled {
		return false
	}
	for _, src := range cfg.Sources {
		if src.AccountID != accountID || !src.Enabled {
			continue
		}
		if (daily && !src.ResetDaily) || (weekly && !src.ResetWeekly) || (monthly && !src.ResetMonthly) {
			return false
		}
		for _, id := range src.TargetSubscriptionIDs {
			if id == targetID {
				return true
			}
		}
	}
	return false
}

type UndoSummary struct {
	EventID        string `json:"event_id"`
	SubscriptionID int64  `json:"subscription_id"`
	UserID         int64  `json:"user_id,omitempty"`
	Status         string `json:"status"`
	Succeeded      int    `json:"succeeded"`
	Failed         int    `json:"failed"`
	Message        string `json:"message,omitempty"`
}

var errUndoEventNotFound = errors.New("指定的重置记录不存在")
var errUndoTargetNotFound = errors.New("指定的下游用户记录不存在")

func (e *Engine) UndoTarget(ctx context.Context, eventID string, subscriptionID int64) (UndoSummary, error) {
	eventID = strings.TrimSpace(eventID)
	summary := UndoSummary{EventID: eventID, SubscriptionID: subscriptionID}
	if eventID == "" || len(eventID) > 200 {
		return summary, errors.New("重置记录编号无效")
	}
	if subscriptionID <= 0 {
		return summary, errors.New("下游订阅编号无效")
	}
	if !e.pollMu.TryLock() {
		return summary, errors.New("轮询或撤销任务正在运行")
	}
	defer e.pollMu.Unlock()
	event, ok := e.store.Event(eventID)
	if !ok {
		return summary, errUndoEventNotFound
	}
	var target TargetResult
	found := false
	for i := range event.Targets {
		if event.Targets[i].SubscriptionID == subscriptionID {
			target = event.Targets[i]
			found = true
			break
		}
	}
	if !found {
		return summary, errUndoTargetNotFound
	}
	if target.BeforeReset != nil {
		summary.UserID = target.BeforeReset.UserID
	} else if target.AfterReset != nil {
		summary.UserID = target.AfterReset.UserID
	}
	if target.Status != "succeeded" {
		return summary, errors.New("该用户没有成功的配额重置，不能撤销")
	}
	if target.UndoStatus == "succeeded" {
		return summary, errors.New("该用户的用量已经撤销")
	}
	expiresAt, err := time.Parse(time.RFC3339, event.UndoExpiresAt)
	if err != nil || !time.Now().Before(expiresAt) {
		_ = e.store.UpdateUndoTarget(event.ID, target.SubscriptionID, "expired", "撤销时限已过")
		e.updateEventUndoAggregate(event.ID, time.Now())
		return summary, errors.New("该用户的重置记录已超过 24 小时撤销时限")
	}
	if e.restorer == nil {
		return summary, errors.New("撤销存储不可用")
	}
	if err := e.store.UpdateUndoTarget(event.ID, target.SubscriptionID, "in_progress", ""); err != nil {
		return summary, fmt.Errorf("标记用户撤销状态: %w", err)
	}
	e.updateEventUndoAggregate(event.ID, time.Now())
	result, restoreErr := e.restorer.Restore(ctx, target, event.ResetDaily, event.ResetWeekly, event.ResetMonthly, time.Now())
	if restoreErr != nil {
		summary.Status = "failed"
		summary.Failed = 1
		summary.Message = restoreErr.Error()
		_ = e.store.UpdateUndoTarget(event.ID, target.SubscriptionID, "failed", restoreErr.Error())
		e.updateEventUndoAggregate(event.ID, time.Now())
		if current, exists := e.store.Event(event.ID); exists {
			e.notifyUndo(current, summary)
		}
		return summary, fmt.Errorf("撤销该用户用量失败: %w", restoreErr)
	}
	summary.Status = "succeeded"
	summary.Succeeded = 1
	summary.Message = result.CacheWarning
	if err := e.store.UpdateUndoTarget(event.ID, target.SubscriptionID, "succeeded", result.CacheWarning); err != nil {
		return summary, fmt.Errorf("记录用户撤销结果: %w", err)
	}
	e.updateEventUndoAggregate(event.ID, time.Now())
	if current, exists := e.store.Event(event.ID); exists {
		e.notifyUndo(current, summary)
	}
	return summary, nil
}

func (e *Engine) updateEventUndoAggregate(eventID string, now time.Time) {
	event, ok := e.store.Event(eventID)
	if !ok {
		return
	}
	total, succeeded, failed, inProgress := 0, 0, 0, 0
	for _, target := range event.Targets {
		if target.Status != "succeeded" {
			continue
		}
		total++
		switch target.UndoStatus {
		case "succeeded":
			succeeded++
		case "failed":
			failed++
		case "in_progress":
			inProgress++
		}
	}
	status, message := "available", ""
	switch {
	case total == 0:
		status, message = "failed", "没有可撤销的成功用户"
	case succeeded == total:
		status, message = "succeeded", fmt.Sprintf("%d/%d 个用户已撤销", succeeded, total)
	case undoDeadlineExpired(event.UndoExpiresAt, now):
		status, message = "expired", fmt.Sprintf("撤销时限已过；%d/%d 个用户已撤销", succeeded, total)
	case inProgress > 0:
		status, message = "in_progress", fmt.Sprintf("正在撤销 1 个用户；%d/%d 个已完成", succeeded, total)
	case failed > 0 && succeeded > 0:
		status, message = "partial", fmt.Sprintf("%d/%d 个用户已撤销，%d 个失败", succeeded, total, failed)
	case failed > 0:
		status, message = "failed", fmt.Sprintf("尚未成功撤销；%d 个用户失败", failed)
	case succeeded > 0:
		status, message = "partial", fmt.Sprintf("%d/%d 个用户已撤销", succeeded, total)
	}
	if err := e.store.UpdateEventUndo(eventID, status, message); err != nil && e.log != nil {
		e.log.Error("update aggregate undo status failed", "event_id", eventID, "error", err)
	}
}

func undoDeadlineExpired(raw string, now time.Time) bool {
	expiresAt, err := time.Parse(time.RFC3339, raw)
	return err != nil || !now.Before(expiresAt)
}

func (e *Engine) notifyDetection(event ResetEvent) {
	cfg := e.store.Config()
	content := buildNotificationContent(cfg, "detection", event, 0, 0)
	e.notifyPhase(event.ID, "detection", cfg, content.Title, content.Body)
}

func (e *Engine) notifyReset(event ResetEvent, succeeded, failed int) {
	cfg := e.store.Config()
	content := buildNotificationContent(cfg, "reset", event, succeeded, failed)
	e.notifyPhase(event.ID, "reset", cfg, content.Title, content.Body)
}

func (e *Engine) notifyUndo(event ResetEvent, summary UndoSummary) {
	cfg := e.store.Config()
	var target *TargetResult
	for i := range event.Targets {
		if event.Targets[i].SubscriptionID == summary.SubscriptionID {
			target = &event.Targets[i]
			break
		}
	}
	content := buildTargetNotificationContent(cfg, "undo", event, summary.Succeeded, summary.Failed, target)
	e.notifyPhase(event.ID, "undo", cfg, content.Title, content.Body)
}

func (e *Engine) notifyPhase(eventID, phase string, cfg Config, title, body string) {
	if e.notifier == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result := e.notifier.SendConfigured(ctx, cfg, title, body)
	if result.Attempted == 0 {
		return
	}
	status := "succeeded"
	if result.Succeeded == 0 {
		status = "failed"
	} else if result.Succeeded < result.Attempted {
		status = "partial"
	}
	message := strings.Join(result.Errors, "；")
	if err := e.store.UpdateEventNotification(eventID, phase, status, message); err != nil {
		e.log.Warn("persist notification result failed", "event_id", eventID, "phase", phase, "error", err)
	}
	if len(result.Errors) > 0 {
		e.log.Warn("notification delivery failed", "event_id", eventID, "phase", phase, "errors", message)
	}
}

func secondsApart(a, b int64) int64 {
	if a >= b {
		return a - b
	}
	return b - a
}

func sameNativeWindowIdentity(prev AccountState, current WeeklySample) bool {
	if !prev.Initialized || prev.ExpectedResetAt <= 0 || current.ResetAt <= 0 || current.WindowSeconds <= 0 {
		return false
	}
	if prev.WindowSeconds != current.WindowSeconds {
		return false
	}
	if prev.UpstreamAccountID != "" && current.UpstreamAccountID != "" && prev.UpstreamAccountID != current.UpstreamAccountID {
		return false
	}
	return prev.PlanType == "" || current.PlanType == "" || prev.PlanType == current.PlanType
}

// stabilizeNativeWindow keeps Sub2API's local 7d aggregation on the previous
// exact boundary when the newly reported reset timestamp only drifts by a few
// seconds. Without this, a usage_log that lies on the boundary can disappear
// from window_stats and imitate an official upstream reset. A material reset
// timestamp change is left untouched so a real staff-triggered reset can still
// establish a new upstream window and be detected.
func (e *Engine) stabilizeNativeWindow(ctx context.Context, accountID int64, prev AccountState, current WeeklySample) (WeeklySample, error) {
	if !sameNativeWindowIdentity(prev, current) || current.ResetAt == prev.ExpectedResetAt {
		return current, nil
	}
	if secondsApart(current.ResetAt, prev.ExpectedResetAt) > int64(nativeWindowDriftTolerance/time.Second) {
		return current, nil
	}
	if e == nil || e.window == nil {
		return WeeklySample{}, errors.New("固定 7d 窗口读取器未初始化")
	}
	startUnix := prev.ExpectedResetAt - current.WindowSeconds
	if startUnix <= 0 {
		return WeeklySample{}, errors.New("固定 7d 窗口起点无效")
	}
	stable, err := e.window.ReadAccountWindowTotals(ctx, accountID, time.Unix(startUnix, 0))
	if err != nil {
		return WeeklySample{}, fmt.Errorf("校正 7d 窗口边界漂移: %w", err)
	}
	observedResetAt := current.ResetAt
	observedUsage := current.Usage
	current.ResetAt = prev.ExpectedResetAt
	current.Usage = stable
	if e.log != nil {
		e.log.Info("normalized native weekly window boundary drift",
			"account_id", accountID,
			"stable_reset_at", current.ResetAt,
			"observed_reset_at", observedResetAt,
			"observed", observedUsage,
			"normalized", stable,
		)
	}
	return current, nil
}

func classify(prev AccountState, current WeeklySample, cfg Config) Decision {
	if !prev.Initialized {
		return DecisionBaseline
	}
	if current.FetchedAt <= prev.LastFetchedAt {
		return DecisionOutOfOrder
	}
	if prev.WindowSeconds != current.WindowSeconds ||
		(prev.UpstreamAccountID != "" && current.UpstreamAccountID != "" && prev.UpstreamAccountID != current.UpstreamAccountID) ||
		(prev.PlanType != "" && current.PlanType != "" && prev.PlanType != current.PlanType) {
		return DecisionSourceChanged
	}
	if prev.ExpectedResetAt > 0 && current.FetchedAt >= prev.ExpectedResetAt-int64(cfg.NaturalGraceSeconds) {
		return DecisionNaturalReset
	}
	// Sub2API's native 7d window_stats are monotonic while the upstream window
	// start is unchanged. A decrease in any exact counter/cost is therefore a
	// candidate even if customers consumed again between polls. The second
	// sample, natural-deadline guard, and reset-credit guard remain fail-closed.
	dropped := usageDropped(prev.LastUsage, current.Usage)
	if !dropped {
		return DecisionSteady
	}
	if prev.ExpectedResetAt <= 0 {
		return DecisionNoDeadline
	}
	if !prev.CreditCountKnown || !current.CreditCountKnown || !prev.CreditDetailsComplete || !current.CreditDetailsComplete {
		return DecisionCreditUnknown
	}
	if current.CreditCount < prev.LastCreditCount {
		return DecisionCreditDecrease
	}
	// A replacement can hide a consumed card behind an unchanged/increased
	// count. Require the current expiry multiset to retain every expiry from the
	// previous sample; pure additions are safe, any disappearance is not.
	if !creditExpirationsRetained(prev.LastCreditExpirations, current.CreditExpirations) {
		return DecisionCreditChanged
	}
	// One confirmed upstream reset may cause only one event for a given reset
	// window. This is a final idempotency fuse behind boundary normalization: a
	// repeated high/low read of the same window cannot reset downstream users
	// again, while a later staff reset with a materially new deadline is allowed.
	if prev.LastConfirmedResetAt > 0 && secondsApart(prev.LastConfirmedResetAt, current.ResetAt) <= int64(nativeWindowDriftTolerance/time.Second) {
		return DecisionDuplicateReset
	}
	return DecisionCandidate
}

func validateSampleAge(sample WeeklySample, maxAgeSeconds int, now time.Time) error {
	age := now.Unix() - sample.FetchedAt
	if age > int64(maxAgeSeconds) {
		return fmt.Errorf("quota sample is stale by %d seconds", age)
	}
	if age < -60 {
		return fmt.Errorf("quota sample is %d seconds in the future", -age)
	}
	return nil
}

func (e *Engine) saveSample(accountID int64, old AccountState, sample WeeklySample, decision Decision, lastErr string) {
	state := AccountState{
		Initialized:           true,
		LastUsage:             sample.Usage,
		LastFetchedAt:         sample.FetchedAt,
		ExpectedResetAt:       sample.ResetAt,
		LastCreditCount:       sample.CreditCount,
		CreditCountKnown:      sample.CreditCountKnown,
		CreditDetailsComplete: sample.CreditDetailsComplete,
		LastCreditExpirations: append([]string(nil), sample.CreditExpirations...),
		UpstreamAccountID:     sample.UpstreamAccountID,
		PlanType:              sample.PlanType,
		WindowSeconds:         sample.WindowSeconds,
		LastCheckedAt:         time.Now().UTC().Format(time.RFC3339),
		LastDecision:          string(decision),
		LastError:             lastErr,
		Generation:            old.Generation,
		LastConfirmedResetAt:  old.LastConfirmedResetAt,
		LastConfirmedAt:       old.LastConfirmedAt,
	}
	if err := e.store.SetAccount(accountID, state); err != nil {
		e.log.Error("persist account state failed", "account_id", accountID, "error", err)
	}
}

func (e *Engine) recordSourceError(accountID int64, err error) {
	state, _ := e.store.Account(accountID)
	state.LastCheckedAt = time.Now().UTC().Format(time.RFC3339)
	state.LastError = err.Error()
	state.LastDecision = "query_error"
	_ = e.store.SetAccount(accountID, state)
}

func (e *Engine) setNext(t time.Time) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	if t.IsZero() {
		e.run.NextPollAt = ""
	} else {
		e.run.NextPollAt = t.UTC().Format(time.RFC3339)
	}
}

func floatsEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.000001
}

func usageDropped(previous, current UsageTotals) bool {
	if current.Requests < previous.Requests || current.Tokens < previous.Tokens {
		return true
	}
	return current.Cost < previous.Cost && !floatsEqual(current.Cost, previous.Cost) ||
		current.StandardCost < previous.StandardCost && !floatsEqual(current.StandardCost, previous.StandardCost) ||
		current.UserCost < previous.UserCost && !floatsEqual(current.UserCost, previous.UserCost)
}

func creditExpirationsRetained(previous, current []string) bool {
	counts := make(map[string]int, len(current))
	for _, expiry := range current {
		counts[expiry]++
	}
	for _, expiry := range previous {
		if counts[expiry] == 0 {
			return false
		}
		counts[expiry]--
	}
	return true
}
