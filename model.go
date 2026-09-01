package main

import "time"

const (
	stateVersion = 6
	undoWindow   = 24 * time.Hour
)

type Config struct {
	Enabled                    bool           `json:"enabled"`
	PollIntervalSeconds        int            `json:"poll_interval_seconds"`
	ConfirmDelaySeconds        int            `json:"confirm_delay_seconds"`
	NaturalGraceSeconds        int            `json:"natural_grace_seconds"`
	MaxSampleAgeSeconds        int            `json:"max_sample_age_seconds"`
	BarkEnabled                bool           `json:"bark_enabled"`
	BarkServerURL              string         `json:"bark_server_url"`
	BarkDeviceKey              string         `json:"bark_device_key,omitempty"`
	BarkGroup                  string         `json:"bark_group"`
	BarkLevel                  string         `json:"bark_level"`
	WeComEnabled               bool           `json:"wecom_enabled"`
	WeComWebhookURL            string         `json:"wecom_webhook_url,omitempty"`
	DetectionNotificationTitle string         `json:"detection_notification_title"`
	DetectionNotificationBody  string         `json:"detection_notification_body"`
	ResetNotificationTitle     string         `json:"reset_notification_title"`
	ResetNotificationBody      string         `json:"reset_notification_body"`
	UndoNotificationTitle      string         `json:"undo_notification_title"`
	UndoNotificationBody       string         `json:"undo_notification_body"`
	Sources                    []SourceConfig `json:"sources"`
}

type SourceConfig struct {
	AccountID             int64   `json:"account_id"`
	AccountName           string  `json:"account_name,omitempty"`
	Enabled               bool    `json:"enabled"`
	TargetSubscriptionIDs []int64 `json:"target_subscription_ids"`
	ResetDaily            bool    `json:"reset_daily"`
	ResetWeekly           bool    `json:"reset_weekly"`
	ResetMonthly          bool    `json:"reset_monthly"`
}

type AccountState struct {
	Initialized           bool        `json:"initialized"`
	LastUsage             UsageTotals `json:"last_usage"`
	LastFetchedAt         int64       `json:"last_fetched_at"`
	ExpectedResetAt       int64       `json:"expected_reset_at"`
	LastCreditCount       int         `json:"last_credit_count"`
	CreditCountKnown      bool        `json:"credit_count_known"`
	CreditDetailsComplete bool        `json:"credit_details_complete"`
	LastCreditExpirations []string    `json:"last_credit_expirations"`
	UpstreamAccountID     string      `json:"upstream_account_id,omitempty"`
	PlanType              string      `json:"plan_type,omitempty"`
	WindowSeconds         int64       `json:"window_seconds"`
	LastCheckedAt         string      `json:"last_checked_at,omitempty"`
	LastDecision          string      `json:"last_decision,omitempty"`
	LastError             string      `json:"last_error,omitempty"`
	Generation            int64       `json:"generation"`
	LastConfirmedResetAt  int64       `json:"last_confirmed_reset_at,omitempty"`
	LastConfirmedAt       string      `json:"last_confirmed_at,omitempty"`
}

type TargetResult struct {
	SubscriptionID int64                      `json:"subscription_id"`
	Status         string                     `json:"status"`
	Message        string                     `json:"message,omitempty"`
	FinishedAt     string                     `json:"finished_at,omitempty"`
	BeforeReset    *SubscriptionUsageSnapshot `json:"before_reset,omitempty"`
	AfterReset     *SubscriptionUsageSnapshot `json:"after_reset,omitempty"`
	UndoStatus     string                     `json:"undo_status,omitempty"`
	UndoMessage    string                     `json:"undo_message,omitempty"`
	UndoneAt       string                     `json:"undone_at,omitempty"`
}

// SubscriptionUsageSnapshot is the minimum state required for a safe undo.
// The post-reset snapshot acts as a compare-and-swap guard: rollback is refused
// if a later manual/natural window reset changed any selected window anchor.
type SubscriptionUsageSnapshot struct {
	SubscriptionID     int64      `json:"subscription_id"`
	UserID             int64      `json:"user_id"`
	UserEmail          string     `json:"user_email,omitempty"`
	Username           string     `json:"username,omitempty"`
	GroupID            int64      `json:"group_id"`
	GroupName          string     `json:"group_name,omitempty"`
	CapturedAt         string     `json:"captured_at"`
	DailyUsageUSD      float64    `json:"daily_usage_usd"`
	WeeklyUsageUSD     float64    `json:"weekly_usage_usd"`
	MonthlyUsageUSD    float64    `json:"monthly_usage_usd"`
	DailyWindowStart   *time.Time `json:"daily_window_start"`
	WeeklyWindowStart  *time.Time `json:"weekly_window_start"`
	MonthlyWindowStart *time.Time `json:"monthly_window_start"`
}

type ResetEvent struct {
	ID                string      `json:"id"`
	SourceAccountID   int64       `json:"source_account_id"`
	SourceAccountName string      `json:"source_account_name,omitempty"`
	DetectedAt        string      `json:"detected_at"`
	PreviousUsage     UsageTotals `json:"previous_usage"`
	CandidateUsage    UsageTotals `json:"candidate_usage"`
	ConfirmedUsage    UsageTotals `json:"confirmed_usage"`
	// Legacy percentage fields are retained only so pre-v4 event history is not
	// lost during migration. New events leave them at zero and use UsageTotals.
	PreviousUsed                 float64  `json:"previous_used_percent,omitempty"`
	CandidateUsed                float64  `json:"candidate_used_percent,omitempty"`
	ConfirmedUsed                float64  `json:"confirmed_used_percent,omitempty"`
	PreviousResetAt              int64    `json:"previous_reset_at"`
	ConfirmedResetAt             int64    `json:"confirmed_reset_at"`
	PreviousCredits              int      `json:"previous_credits"`
	ConfirmedCredits             int      `json:"confirmed_credits"`
	PreviousCreditExpirations    []string `json:"previous_credit_expirations,omitempty"`
	ConfirmedCreditExpirations   []string `json:"confirmed_credit_expirations,omitempty"`
	ResetDaily                   bool     `json:"reset_daily"`
	ResetWeekly                  bool     `json:"reset_weekly"`
	ResetMonthly                 bool     `json:"reset_monthly"`
	SuppressedReason             string   `json:"suppressed_reason,omitempty"`
	UndoExpiresAt                string   `json:"undo_expires_at,omitempty"`
	UndoStatus                   string   `json:"undo_status,omitempty"`
	UndoMessage                  string   `json:"undo_message,omitempty"`
	DetectionNotificationStatus  string   `json:"detection_notification_status,omitempty"`
	DetectionNotificationMessage string   `json:"detection_notification_message,omitempty"`
	ResetNotificationStatus      string   `json:"reset_notification_status,omitempty"`
	ResetNotificationMessage     string   `json:"reset_notification_message,omitempty"`
	UndoNotificationStatus       string   `json:"undo_notification_status,omitempty"`
	UndoNotificationMessage      string   `json:"undo_notification_message,omitempty"`
	// Kept for pre-v5 history. New events use the phase-specific fields above.
	NotificationStatus  string         `json:"notification_status,omitempty"`
	NotificationMessage string         `json:"notification_message,omitempty"`
	Targets             []TargetResult `json:"targets"`
}

type PersistentState struct {
	Version       int                     `json:"version"`
	Config        Config                  `json:"config"`
	Accounts      map[string]AccountState `json:"accounts"`
	Events        []ResetEvent            `json:"events"`
	PollSucceeded int64                   `json:"poll_succeeded"`
	PollFailed    int64                   `json:"poll_failed"`
	UpdatedAt     string                  `json:"updated_at"`
}

func defaultConfig() Config {
	return Config{
		Enabled:                    false,
		PollIntervalSeconds:        60,
		ConfirmDelaySeconds:        10,
		NaturalGraceSeconds:        120,
		MaxSampleAgeSeconds:        300,
		BarkEnabled:                false,
		BarkServerURL:              "https://api.day.app",
		BarkGroup:                  "Sub2API 自动重置",
		BarkLevel:                  "active",
		WeComEnabled:               false,
		DetectionNotificationTitle: defaultDetectionNotificationTitle,
		DetectionNotificationBody:  defaultDetectionNotificationBody,
		ResetNotificationTitle:     defaultResetNotificationTitle,
		ResetNotificationBody:      defaultResetNotificationBody,
		UndoNotificationTitle:      defaultUndoNotificationTitle,
		UndoNotificationBody:       defaultUndoNotificationBody,
		Sources:                    []SourceConfig{},
	}
}

type WeeklySample struct {
	Usage                 UsageTotals
	FetchedAt             int64
	ResetAt               int64
	WindowSeconds         int64
	ForcedRefresh         bool
	CreditCount           int
	CreditCountKnown      bool
	CreditDetailsComplete bool
	CreditExpirations     []string
	UpstreamAccountID     string
	PlanType              string
}

// UsageTotals is the exact, unformatted 7d window_stats payload returned by
// Sub2API's own account-usage endpoint. These are cumulative values from the
// current upstream window start, not percentages inferred by the sidecar.
type UsageTotals struct {
	Requests     int64   `json:"requests"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
	StandardCost float64 `json:"standard_cost"`
	UserCost     float64 `json:"user_cost"`
}

type Decision string

const (
	DecisionBaseline       Decision = "baseline"
	DecisionSteady         Decision = "steady"
	DecisionCandidate      Decision = "manual_reset_candidate"
	DecisionNaturalReset   Decision = "natural_cycle_reset"
	DecisionCreditDecrease Decision = "reset_credit_decreased"
	DecisionCreditChanged  Decision = "reset_credit_details_changed"
	DecisionCreditUnknown  Decision = "drop_ignored_credit_unknown"
	DecisionNoDeadline     Decision = "drop_ignored_no_natural_deadline"
	DecisionDuplicateReset Decision = "duplicate_confirmed_reset"
	DecisionSourceChanged  Decision = "source_or_window_changed"
	DecisionOutOfOrder     Decision = "out_of_order_sample"
)

type EngineRuntime struct {
	Running        bool         `json:"running"`
	LastStartedAt  string       `json:"last_started_at,omitempty"`
	LastFinishedAt string       `json:"last_finished_at,omitempty"`
	LastError      string       `json:"last_error,omitempty"`
	NextPollAt     string       `json:"next_poll_at,omitempty"`
	LastResult     *PollSummary `json:"last_result,omitempty"`
}

type PollSummary struct {
	Status              string `json:"status"`
	Message             string `json:"message"`
	Sources             int    `json:"sources"`
	Checked             int    `json:"checked"`
	Failed              int    `json:"failed"`
	Detected            int    `json:"detected"`
	DownstreamSucceeded int    `json:"downstream_succeeded"`
	DownstreamFailed    int    `json:"downstream_failed"`
}

type Account struct {
	ID              int64                 `json:"id"`
	Name            string                `json:"name"`
	Platform        string                `json:"platform"`
	Type            string                `json:"type"`
	Status          string                `json:"status"`
	Credentials     map[string]any        `json:"credentials,omitempty"`
	Extra           map[string]any        `json:"extra,omitempty"`
	UpdatedAt       time.Time             `json:"updated_at"`
	ParentAccountID *int64                `json:"parent_account_id"`
	QuotaDimension  string                `json:"quota_dimension"`
	GroupIDs        []int64               `json:"group_ids,omitempty"`
	AccountGroups   []AccountGroupSummary `json:"account_groups,omitempty"`
}

type AccountGroupSummary struct {
	GroupID int64 `json:"group_id"`
}

func (a Account) mappingGroupIDs() map[int64]struct{} {
	result := make(map[int64]struct{}, len(a.GroupIDs)+len(a.AccountGroups))
	for _, id := range a.GroupIDs {
		if id > 0 {
			result[id] = struct{}{}
		}
	}
	for _, relation := range a.AccountGroups {
		if relation.GroupID > 0 {
			result[relation.GroupID] = struct{}{}
		}
	}
	return result
}

type UserSummary struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

type GroupSummary struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

type Subscription struct {
	ID                 int64         `json:"id"`
	UserID             int64         `json:"user_id"`
	GroupID            int64         `json:"group_id"`
	Status             string        `json:"status"`
	DailyWindowStart   *time.Time    `json:"daily_window_start"`
	WeeklyWindowStart  *time.Time    `json:"weekly_window_start"`
	MonthlyWindowStart *time.Time    `json:"monthly_window_start"`
	DailyUsageUSD      float64       `json:"daily_usage_usd"`
	WeeklyUsageUSD     float64       `json:"weekly_usage_usd"`
	MonthlyUsageUSD    float64       `json:"monthly_usage_usd"`
	UpdatedAt          time.Time     `json:"updated_at"`
	User               *UserSummary  `json:"user,omitempty"`
	Group              *GroupSummary `json:"group,omitempty"`
}

func snapshotSubscription(sub Subscription, capturedAt time.Time) SubscriptionUsageSnapshot {
	snapshot := SubscriptionUsageSnapshot{
		SubscriptionID:     sub.ID,
		UserID:             sub.UserID,
		GroupID:            sub.GroupID,
		CapturedAt:         capturedAt.UTC().Format(time.RFC3339Nano),
		DailyUsageUSD:      sub.DailyUsageUSD,
		WeeklyUsageUSD:     sub.WeeklyUsageUSD,
		MonthlyUsageUSD:    sub.MonthlyUsageUSD,
		DailyWindowStart:   cloneTime(sub.DailyWindowStart),
		WeeklyWindowStart:  cloneTime(sub.WeeklyWindowStart),
		MonthlyWindowStart: cloneTime(sub.MonthlyWindowStart),
	}
	if sub.User != nil {
		snapshot.UserEmail = sub.User.Email
		snapshot.Username = sub.User.Username
	}
	if sub.Group != nil {
		snapshot.GroupName = sub.Group.Name
	}
	return snapshot
}

func cloneTime(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func unixString(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
