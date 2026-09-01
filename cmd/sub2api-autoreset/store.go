package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu   sync.RWMutex
	path string
	data PersistentState
}

func OpenStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("state path is empty")
	}
	s := &Store{path: path}
	s.data = PersistentState{
		Version:  stateVersion,
		Config:   defaultConfig(),
		Accounts: map[string]AccountState{},
		Events:   []ResetEvent{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read state: %w", err)
		}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if s.data.Version < 1 || s.data.Version > stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", s.data.Version)
	}
	loadedVersion := s.data.Version
	if s.data.Accounts == nil {
		s.data.Accounts = map[string]AccountState{}
	}
	if s.data.Events == nil {
		s.data.Events = []ResetEvent{}
	}
	if loadedVersion <= 2 {
		// Version 3 compares the exact upstream 7d value instead of applying a
		// fixed percentage-point threshold. Rebuild every existing baseline once
		// so changing the detector cannot turn an old sample into a reset event.
		for id, account := range s.data.Accounts {
			account.Initialized = false
			account.LastDecision = "algorithm_upgrade_rebaseline"
			s.data.Accounts[id] = account
		}
	}
	if loadedVersion <= 3 {
		// Version 4 changes the detector's source of truth from the upstream
		// percentage to Sub2API's own exact 7d window_stats. The two baselines are
		// not comparable, so every monitored account must establish one fresh
		// native-usage sample before it can emit an event.
		for id, account := range s.data.Accounts {
			account.Initialized = false
			account.LastDecision = "native_usage_upgrade_rebaseline"
			s.data.Accounts[id] = account
		}
	}
	if loadedVersion <= 4 {
		// Version 5 makes rollback protection unconditional and fixes its lifetime
		// at 24 hours. Existing successful events receive the same fixed window.
		for i := range s.data.Events {
			event := &s.data.Events[i]
			hasSuccessfulTarget := false
			for _, target := range event.Targets {
				if target.Status == "succeeded" {
					hasSuccessfulTarget = true
					break
				}
			}
			if !hasSuccessfulTarget || event.UndoStatus == "succeeded" {
				continue
			}
			detectedAt, err := time.Parse(time.RFC3339, event.DetectedAt)
			if err == nil {
				event.UndoExpiresAt = detectedAt.Add(undoWindow).UTC().Format(time.RFC3339)
			}
		}
	}
	if loadedVersion <= 5 {
		// Version 6 adds an idempotency fuse for a confirmed upstream window.
		// Seed it from the newest successful historical event so restarting after
		// an upgrade cannot replay the same reset against downstream users.
		type confirmedEvent struct {
			resetAt    int64
			detectedAt time.Time
		}
		latest := make(map[int64]confirmedEvent)
		for _, event := range s.data.Events {
			if event.SourceAccountID <= 0 || event.ConfirmedResetAt <= 0 {
				continue
			}
			succeeded := false
			for _, target := range event.Targets {
				if target.Status == "succeeded" {
					succeeded = true
					break
				}
			}
			if !succeeded {
				continue
			}
			detectedAt, err := time.Parse(time.RFC3339, event.DetectedAt)
			if err != nil {
				continue
			}
			current, ok := latest[event.SourceAccountID]
			if !ok || detectedAt.After(current.detectedAt) {
				latest[event.SourceAccountID] = confirmedEvent{resetAt: event.ConfirmedResetAt, detectedAt: detectedAt}
			}
		}
		for accountID, event := range latest {
			key := fmt.Sprintf("%d", accountID)
			account, ok := s.data.Accounts[key]
			if !ok {
				continue
			}
			account.LastConfirmedResetAt = event.resetAt
			account.LastConfirmedAt = event.detectedAt.UTC().Format(time.RFC3339)
			s.data.Accounts[key] = account
		}
	}
	applyConfigDefaults(&s.data.Config)
	if loadedVersion != stateVersion {
		s.data.Version = stateVersion
		if err := s.saveLocked(); err != nil {
			return nil, fmt.Errorf("migrate state: %w", err)
		}
	}
	return s, nil
}

func applyConfigDefaults(c *Config) {
	d := defaultConfig()
	if c.PollIntervalSeconds == 0 {
		c.PollIntervalSeconds = d.PollIntervalSeconds
	}
	if c.ConfirmDelaySeconds == 0 {
		c.ConfirmDelaySeconds = d.ConfirmDelaySeconds
	}
	if c.NaturalGraceSeconds == 0 {
		c.NaturalGraceSeconds = d.NaturalGraceSeconds
	}
	if c.MaxSampleAgeSeconds == 0 {
		c.MaxSampleAgeSeconds = d.MaxSampleAgeSeconds
	}
	if strings.TrimSpace(c.BarkServerURL) == "" {
		c.BarkServerURL = d.BarkServerURL
	}
	if strings.TrimSpace(c.BarkGroup) == "" {
		c.BarkGroup = d.BarkGroup
	}
	if strings.TrimSpace(c.BarkLevel) == "" {
		c.BarkLevel = d.BarkLevel
	}
	if strings.TrimSpace(c.DetectionNotificationTitle) == "" {
		c.DetectionNotificationTitle = d.DetectionNotificationTitle
	}
	if strings.TrimSpace(c.DetectionNotificationBody) == "" {
		c.DetectionNotificationBody = d.DetectionNotificationBody
	}
	if strings.TrimSpace(c.ResetNotificationTitle) == "" {
		c.ResetNotificationTitle = d.ResetNotificationTitle
	}
	if strings.TrimSpace(c.ResetNotificationBody) == "" {
		c.ResetNotificationBody = d.ResetNotificationBody
	}
	if strings.TrimSpace(c.UndoNotificationTitle) == "" {
		c.UndoNotificationTitle = d.UndoNotificationTitle
	}
	if strings.TrimSpace(c.UndoNotificationBody) == "" || strings.TrimSpace(c.UndoNotificationBody) == legacyDefaultUndoNotificationBody {
		c.UndoNotificationBody = d.UndoNotificationBody
	}
	if c.Sources == nil {
		c.Sources = []SourceConfig{}
	}
}

func validateConfig(c *Config) error {
	if c == nil {
		return errors.New("配置不能为空")
	}
	if c.PollIntervalSeconds < 10 || c.PollIntervalSeconds > 3600 {
		return errors.New("轮询周期必须在 10–3600 秒之间")
	}
	if c.ConfirmDelaySeconds < 2 || c.ConfirmDelaySeconds > 60 {
		return errors.New("二次确认延迟必须在 2–60 秒之间")
	}
	if c.NaturalGraceSeconds < 0 || c.NaturalGraceSeconds > 900 {
		return errors.New("自然重置保护窗口必须在 0–900 秒之间")
	}
	if c.MaxSampleAgeSeconds < 30 || c.MaxSampleAgeSeconds > 3600 {
		return errors.New("样本最大年龄必须在 30–3600 秒之间")
	}
	c.BarkServerURL = strings.TrimRight(strings.TrimSpace(c.BarkServerURL), "/")
	c.BarkDeviceKey = strings.TrimSpace(c.BarkDeviceKey)
	c.BarkGroup = strings.TrimSpace(c.BarkGroup)
	c.BarkLevel = strings.TrimSpace(c.BarkLevel)
	c.WeComWebhookURL = strings.TrimSpace(c.WeComWebhookURL)
	normalizeNotificationTemplates(c)
	if err := validateNotificationTemplates(*c); err != nil {
		return err
	}
	if c.BarkEnabled {
		if err := validateBarkURL(c.BarkServerURL); err != nil {
			return err
		}
		if c.BarkDeviceKey == "" {
			return errors.New("启用 Bark 通知前必须填写 Device Key")
		}
	}
	if len(c.BarkDeviceKey) > 512 {
		return errors.New("Bark Device Key 过长")
	}
	if len(c.BarkGroup) > 100 {
		return errors.New("Bark 分组名称不能超过 100 个字符")
	}
	switch c.BarkLevel {
	case "active", "timeSensitive", "passive":
	default:
		return errors.New("Bark 通知级别无效")
	}
	if c.WeComEnabled {
		if err := validateWeComWebhookURL(c.WeComWebhookURL); err != nil {
			return err
		}
	}
	if len(c.WeComWebhookURL) > 4096 {
		return errors.New("企业微信 Webhook 地址过长")
	}
	if len(c.Sources) > 200 {
		return errors.New("监听账号数量不能超过 200")
	}
	seenSources := map[int64]bool{}
	for i := range c.Sources {
		src := &c.Sources[i]
		src.AccountName = strings.TrimSpace(src.AccountName)
		if len(src.AccountName) > 200 {
			src.AccountName = src.AccountName[:200]
		}
		if src.AccountID <= 0 {
			return errors.New("监听账号 ID 必须大于 0")
		}
		if seenSources[src.AccountID] {
			return fmt.Errorf("监听账号 %d 重复", src.AccountID)
		}
		seenSources[src.AccountID] = true
		if src.Enabled && len(src.TargetSubscriptionIDs) > 0 && !src.ResetDaily && !src.ResetWeekly && !src.ResetMonthly {
			return fmt.Errorf("监听账号 %d 至少要选择一个下游重置维度", src.AccountID)
		}
		if len(src.TargetSubscriptionIDs) > 2000 {
			return fmt.Errorf("监听账号 %d 的目标订阅不能超过 2000 个", src.AccountID)
		}
		seenTargets := map[int64]bool{}
		clean := make([]int64, 0, len(src.TargetSubscriptionIDs))
		for _, id := range src.TargetSubscriptionIDs {
			if id <= 0 {
				return fmt.Errorf("监听账号 %d 包含无效订阅 ID", src.AccountID)
			}
			if !seenTargets[id] {
				seenTargets[id] = true
				clean = append(clean, id)
			}
		}
		sort.Slice(clean, func(i, j int) bool { return clean[i] < clean[j] })
		src.TargetSubscriptionIDs = clean
	}
	sort.Slice(c.Sources, func(i, j int) bool { return c.Sources[i].AccountID < c.Sources[j].AccountID })
	return nil
}

func (s *Store) Snapshot() PersistentState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.data)
}

func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.data.Config)
}

func (s *Store) SetConfig(c Config) error {
	if err := validateConfig(&c); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Config = cloneConfig(c)
	return s.saveLocked()
}

func (s *Store) Account(id int64) (AccountState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data.Accounts[fmt.Sprint(id)]
	return v, ok
}

func (s *Store) SetAccount(id int64, state AccountState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Accounts[fmt.Sprint(id)] = state
	return s.saveLocked()
}

func (s *Store) AddPollResult(succeeded, failed int) error {
	if succeeded < 0 || failed < 0 {
		return errors.New("轮询计数不能为负数")
	}
	if succeeded == 0 && failed == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.PollSucceeded += int64(succeeded)
	s.data.PollFailed += int64(failed)
	return s.saveLocked()
}

func (s *Store) AddEvent(event ResetEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Events = append([]ResetEvent{event}, s.data.Events...)
	if len(s.data.Events) > 100 {
		s.data.Events = s.data.Events[:100]
	}
	return s.saveLocked()
}

func (s *Store) UpdateTarget(eventID string, subscriptionID int64, status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Events {
		if s.data.Events[i].ID != eventID {
			continue
		}
		for j := range s.data.Events[i].Targets {
			t := &s.data.Events[i].Targets[j]
			if t.SubscriptionID == subscriptionID {
				t.Status = status
				t.Message = strings.TrimSpace(message)
				if len(t.Message) > 500 {
					t.Message = t.Message[:500]
				}
				t.FinishedAt = time.Now().UTC().Format(time.RFC3339)
				return s.saveLocked()
			}
		}
	}
	return fmt.Errorf("event target not found")
}

func (s *Store) PrepareTarget(eventID string, subscriptionID int64, snapshot SubscriptionUsageSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := findTarget(&s.data, eventID, subscriptionID)
	if err != nil {
		return err
	}
	target.Status = "resetting"
	target.Message = ""
	target.BeforeReset = cloneUsageSnapshot(&snapshot)
	return s.saveLocked()
}

func (s *Store) CompleteTarget(eventID string, subscriptionID int64, status, message string, after *SubscriptionUsageSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := findTarget(&s.data, eventID, subscriptionID)
	if err != nil {
		return err
	}
	target.Status = status
	target.Message = trimMessage(message)
	target.AfterReset = cloneUsageSnapshot(after)
	target.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	return s.saveLocked()
}

func (s *Store) UpdateUndoTarget(eventID string, subscriptionID int64, status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := findTarget(&s.data, eventID, subscriptionID)
	if err != nil {
		return err
	}
	target.UndoStatus = status
	target.UndoMessage = trimMessage(message)
	if status == "succeeded" {
		target.UndoneAt = time.Now().UTC().Format(time.RFC3339)
	}
	return s.saveLocked()
}

func (s *Store) UpdateEventUndo(eventID, status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, err := findEvent(&s.data, eventID)
	if err != nil {
		return err
	}
	event.UndoStatus = status
	event.UndoMessage = trimMessage(message)
	return s.saveLocked()
}

func (s *Store) UpdateEventNotification(eventID, phase, status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, err := findEvent(&s.data, eventID)
	if err != nil {
		return err
	}
	message = trimMessage(message)
	switch phase {
	case "detection":
		event.DetectionNotificationStatus = status
		event.DetectionNotificationMessage = message
	case "reset":
		event.ResetNotificationStatus = status
		event.ResetNotificationMessage = message
	case "undo":
		event.UndoNotificationStatus = status
		event.UndoNotificationMessage = message
	default:
		return fmt.Errorf("unknown notification phase %q", phase)
	}
	return s.saveLocked()
}

func (s *Store) Event(eventID string) (ResetEvent, bool) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return ResetEvent{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.data.Events {
		if s.data.Events[i].ID == eventID {
			return cloneEvent(s.data.Events[i]), true
		}
	}
	return ResetEvent{}, false
}

func findEvent(state *PersistentState, eventID string) (*ResetEvent, error) {
	for i := range state.Events {
		if state.Events[i].ID == eventID {
			return &state.Events[i], nil
		}
	}
	return nil, fmt.Errorf("event not found")
}

func findTarget(state *PersistentState, eventID string, subscriptionID int64) (*TargetResult, error) {
	event, err := findEvent(state, eventID)
	if err != nil {
		return nil, err
	}
	for i := range event.Targets {
		if event.Targets[i].SubscriptionID == subscriptionID {
			return &event.Targets[i], nil
		}
	}
	return nil, fmt.Errorf("event target not found")
}

func trimMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}

func cloneUsageSnapshot(in *SubscriptionUsageSnapshot) *SubscriptionUsageSnapshot {
	if in == nil {
		return nil
	}
	b, _ := json.Marshal(in)
	var out SubscriptionUsageSnapshot
	_ = json.Unmarshal(b, &out)
	return &out
}

func cloneEvent(in ResetEvent) ResetEvent {
	b, _ := json.Marshal(in)
	var out ResetEvent
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *Store) saveLocked() error {
	s.data.Version = stateVersion
	s.data.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	ok = true
	return nil
}

func cloneState(in PersistentState) PersistentState {
	b, _ := json.Marshal(in)
	var out PersistentState
	_ = json.Unmarshal(b, &out)
	return out
}

func cloneConfig(in Config) Config {
	b, _ := json.Marshal(in)
	var out Config
	_ = json.Unmarshal(b, &out)
	return out
}
