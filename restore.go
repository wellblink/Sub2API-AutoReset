package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const subscriptionCacheInvalidateChannel = "subscription:cache:invalidate"

type RestoreResult struct {
	CacheWarning string
}

type restoreWindowPlan struct {
	Daily   bool
	Weekly  bool
	Monthly bool
}

func (p restoreWindowPlan) empty() bool {
	return !p.Daily && !p.Weekly && !p.Monthly
}

type UsageRestorer struct {
	db    *pgxpool.Pool
	redis *redis.Client
}

func NewUsageRestorer(ctx context.Context) (*UsageRestorer, error) {
	dbConfig, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fmt.Errorf("create database config: %w", err)
	}
	dbConfig.ConnConfig.Host = envOr("DATABASE_HOST", "postgres")
	dbConfig.ConnConfig.Port = uint16(envInt("DATABASE_PORT", 5432))
	dbConfig.ConnConfig.User = firstEnv("DATABASE_USER", "POSTGRES_USER", "sub2api")
	dbConfig.ConnConfig.Password = firstEnv("DATABASE_PASSWORD", "POSTGRES_PASSWORD", "")
	dbConfig.ConnConfig.Database = firstEnv("DATABASE_DBNAME", "POSTGRES_DB", "sub2api")
	dbConfig.MaxConns = 2
	dbConfig.MinConns = 0
	dbConfig.MaxConnLifetime = 10 * time.Minute
	dbConfig.MaxConnIdleTime = 2 * time.Minute
	if strings.EqualFold(envOr("DATABASE_SSLMODE", "disable"), "disable") {
		dbConfig.ConnConfig.TLSConfig = nil
	} else {
		dbConfig.ConnConfig.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	db, err := pgxpool.NewWithConfig(ctx, dbConfig)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	redisOptions := &redis.Options{
		Addr:     envOr("REDIS_HOST", "redis") + ":" + strconv.Itoa(envInt("REDIS_PORT", 6379)),
		Username: osEnv("REDIS_USERNAME"),
		Password: osEnv("REDIS_PASSWORD"),
		DB:       envInt("REDIS_DB", 0),
	}
	if strings.EqualFold(osEnv("REDIS_ENABLE_TLS"), "true") {
		redisOptions.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	redisClient := redis.NewClient(redisOptions)
	if err := redisClient.Ping(ctx).Err(); err != nil {
		db.Close()
		_ = redisClient.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &UsageRestorer{db: db, redis: redisClient}, nil
}

func osEnv(name string) string {
	return strings.TrimSpace(getenv(name))
}

func firstEnv(primary, secondary, fallback string) string {
	if value := osEnv(primary); value != "" {
		return value
	}
	if value := osEnv(secondary); value != "" {
		return value
	}
	return fallback
}

var getenv = func(name string) string {
	return os.Getenv(name)
}

func envInt(name string, fallback int) int {
	raw := osEnv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > 65535 {
		return fallback
	}
	return value
}

func (r *UsageRestorer) Close() {
	if r == nil {
		return
	}
	if r.db != nil {
		r.db.Close()
	}
	if r.redis != nil {
		_ = r.redis.Close()
	}
}

type currentSubscriptionUsage struct {
	UserID             int64
	GroupID            int64
	DailyWindowStart   *time.Time
	WeeklyWindowStart  *time.Time
	MonthlyWindowStart *time.Time
	DailyUsageUSD      float64
	WeeklyUsageUSD     float64
	MonthlyUsageUSD    float64
}

// ReadAccountWindowTotals mirrors Sub2API's native window_stats query while
// allowing the detector to keep one stable window boundary. OpenAI's
// reset-after header may move by a second between probes; querying the exact
// previous boundary prevents a record on that boundary from disappearing and
// looking like an upstream quota reset.
func (r *UsageRestorer) ReadAccountWindowTotals(ctx context.Context, accountID int64, startTime time.Time) (UsageTotals, error) {
	if r == nil || r.db == nil {
		return UsageTotals{}, errors.New("用量存储未初始化")
	}
	if accountID <= 0 || startTime.IsZero() {
		return UsageTotals{}, errors.New("用量窗口参数无效")
	}
	var totals UsageTotals
	err := r.db.QueryRow(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0),
			COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0),
			COALESCE(SUM(total_cost), 0),
			COALESCE(SUM(actual_cost), 0)
		FROM usage_logs
		WHERE account_id = $1 AND created_at >= $2`, accountID, startTime).Scan(
		&totals.Requests,
		&totals.Tokens,
		&totals.Cost,
		&totals.StandardCost,
		&totals.UserCost,
	)
	if err != nil {
		return UsageTotals{}, fmt.Errorf("读取固定边界用量: %w", err)
	}
	return totals, nil
}

func (r *UsageRestorer) Restore(ctx context.Context, target TargetResult, daily, weekly, monthly bool, now time.Time) (RestoreResult, error) {
	if r == nil || r.db == nil || r.redis == nil {
		return RestoreResult{}, errors.New("撤销存储未初始化")
	}
	if target.BeforeReset == nil || target.AfterReset == nil {
		return RestoreResult{}, errors.New("该目标缺少重置前后快照")
	}
	before, after := target.BeforeReset, target.AfterReset
	if before.SubscriptionID != target.SubscriptionID || after.SubscriptionID != target.SubscriptionID ||
		before.UserID != after.UserID || before.GroupID != after.GroupID {
		return RestoreResult{}, errors.New("快照身份不一致")
	}
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RestoreResult{}, fmt.Errorf("开始撤销事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current currentSubscriptionUsage
	err = tx.QueryRow(ctx, `
		SELECT user_id, group_id,
		       daily_window_start, weekly_window_start, monthly_window_start,
		       daily_usage_usd, weekly_usage_usd, monthly_usage_usd
		  FROM user_subscriptions
		 WHERE id = $1 AND deleted_at IS NULL
		 FOR UPDATE`, target.SubscriptionID).Scan(
		&current.UserID, &current.GroupID,
		&current.DailyWindowStart, &current.WeeklyWindowStart, &current.MonthlyWindowStart,
		&current.DailyUsageUSD, &current.WeeklyUsageUSD, &current.MonthlyUsageUSD,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RestoreResult{}, errors.New("订阅不存在或已删除")
		}
		return RestoreResult{}, fmt.Errorf("读取当前订阅: %w", err)
	}
	if current.UserID != before.UserID || current.GroupID != before.GroupID {
		return RestoreResult{}, errors.New("订阅所属用户或分组已改变")
	}
	plan, err := resolveRestoreWindows(current, *before, *after, daily, weekly, monthly, now)
	if err != nil {
		return RestoreResult{}, err
	}

	_, err = tx.Exec(ctx, `
		UPDATE user_subscriptions
		   SET daily_usage_usd = CASE WHEN $2 THEN $3 + daily_usage_usd ELSE daily_usage_usd END,
		       daily_window_start = CASE WHEN $2 THEN $4::timestamptz ELSE daily_window_start END,
		       weekly_usage_usd = CASE WHEN $5 THEN $6 + weekly_usage_usd ELSE weekly_usage_usd END,
		       weekly_window_start = CASE WHEN $5 THEN $7::timestamptz ELSE weekly_window_start END,
		       monthly_usage_usd = CASE WHEN $8 THEN $9 + monthly_usage_usd ELSE monthly_usage_usd END,
		       monthly_window_start = CASE WHEN $8 THEN $10::timestamptz ELSE monthly_window_start END,
		       updated_at = NOW()
		 WHERE id = $1`,
		target.SubscriptionID,
		plan.Daily, before.DailyUsageUSD, before.DailyWindowStart,
		plan.Weekly, before.WeeklyUsageUSD, before.WeeklyWindowStart,
		plan.Monthly, before.MonthlyUsageUSD, before.MonthlyWindowStart,
	)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("恢复订阅用量: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RestoreResult{}, fmt.Errorf("提交撤销事务: %w", err)
	}

	cacheKey := fmt.Sprintf("billing:sub:%d:%d", before.UserID, before.GroupID)
	l1Key := fmt.Sprintf("sub:%d:%d", before.UserID, before.GroupID)
	pipe := r.redis.Pipeline()
	pipe.Del(ctx, cacheKey)
	pipe.Publish(ctx, subscriptionCacheInvalidateChannel, l1Key)
	_, cacheErr := pipe.Exec(ctx)
	if cacheErr != nil {
		return RestoreResult{CacheWarning: "数据已恢复，但缓存失效失败；请暂时不要继续消费并检查 Redis"}, nil
	}
	return RestoreResult{}, nil
}

func resolveRestoreWindows(current currentSubscriptionUsage, before, after SubscriptionUsageSnapshot, daily, weekly, monthly bool, now time.Time) (restoreWindowPlan, error) {
	var plan restoreWindowPlan
	if daily {
		// Sub2API keeps daily quota on calendar-day boundaries. If either the
		// reset itself or a later midnight advanced the anchor, the old daily
		// usage belongs only to the enclosing weekly/monthly windows.
		if before.DailyWindowStart != nil &&
			sameLocalCalendarDay(*before.DailyWindowStart, now) &&
			sameTime(before.DailyWindowStart, after.DailyWindowStart) &&
			sameTime(current.DailyWindowStart, after.DailyWindowStart) {
			plan.Daily = true
		}
	}
	if weekly {
		if before.WeeklyWindowStart != nil && now.Before(before.WeeklyWindowStart.Add(7*24*time.Hour)) {
			if !sameTime(current.WeeklyWindowStart, after.WeeklyWindowStart) {
				return plan, errors.New("周窗口在原周期内再次变化，拒绝覆盖新数据")
			}
			plan.Weekly = true
		}
	}
	if monthly {
		if before.MonthlyWindowStart != nil && now.Before(before.MonthlyWindowStart.Add(30*24*time.Hour)) {
			if !sameTime(current.MonthlyWindowStart, after.MonthlyWindowStart) {
				return plan, errors.New("月窗口在原周期内再次变化，拒绝覆盖新数据")
			}
			plan.Monthly = true
		}
	}
	if plan.empty() {
		return plan, errors.New("原额度周期均已自然到期，无需回滚")
	}
	return plan, nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func sameLocalCalendarDay(a, b time.Time) bool {
	a = a.In(time.Local)
	b = b.In(time.Local)
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
