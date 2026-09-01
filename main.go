package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Server struct {
	store    *Store
	client   *APIClient
	engine   *Engine
	notifier *Notifier
	logger   *slog.Logger
}

const (
	configScopeAll           = "all"
	configScopeOverview      = "overview"
	configScopeMapping       = "mapping"
	configScopeNotifications = "notifications"
)

type configUpdateRequest struct {
	Config
	Scope                    string   `json:"scope,omitempty"`
	ClearBarkDeviceKey       bool     `json:"clear_bark_device_key"`
	ClearWeComWebhookURL     bool     `json:"clear_wecom_webhook_url"`
	LegacyDropEpsilonPercent *float64 `json:"drop_epsilon_percent,omitempty"`
}

func mergeScopedConfig(existing Config, req configUpdateRequest) (Config, string, error) {
	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = configScopeAll
	}
	var cfg Config
	switch scope {
	case configScopeAll:
		cfg = req.Config
	case configScopeOverview:
		cfg = existing
		cfg.Enabled = req.Enabled
		cfg.PollIntervalSeconds = req.PollIntervalSeconds
		cfg.ConfirmDelaySeconds = req.ConfirmDelaySeconds
		cfg.NaturalGraceSeconds = req.NaturalGraceSeconds
		cfg.MaxSampleAgeSeconds = req.MaxSampleAgeSeconds
	case configScopeMapping:
		cfg = existing
		cfg.Sources = cloneConfig(req.Config).Sources
	case configScopeNotifications:
		cfg = existing
		cfg.BarkEnabled = req.BarkEnabled
		cfg.BarkServerURL = req.BarkServerURL
		cfg.BarkDeviceKey = req.BarkDeviceKey
		cfg.BarkGroup = req.BarkGroup
		cfg.BarkLevel = req.BarkLevel
		cfg.WeComEnabled = req.WeComEnabled
		cfg.WeComWebhookURL = req.WeComWebhookURL
		cfg.DetectionNotificationTitle = req.DetectionNotificationTitle
		cfg.DetectionNotificationBody = req.DetectionNotificationBody
		cfg.ResetNotificationTitle = req.ResetNotificationTitle
		cfg.ResetNotificationBody = req.ResetNotificationBody
		cfg.UndoNotificationTitle = req.UndoNotificationTitle
		cfg.UndoNotificationBody = req.UndoNotificationBody
	default:
		return Config{}, "", fmt.Errorf("未知的设置保存范围 %q", scope)
	}

	if scope == configScopeAll || scope == configScopeNotifications {
		if req.ClearBarkDeviceKey {
			cfg.BarkDeviceKey = ""
			cfg.BarkEnabled = false
		} else if strings.TrimSpace(cfg.BarkDeviceKey) == "" {
			cfg.BarkDeviceKey = existing.BarkDeviceKey
		}
		if req.ClearWeComWebhookURL {
			cfg.WeComWebhookURL = ""
			cfg.WeComEnabled = false
		} else if strings.TrimSpace(cfg.WeComWebhookURL) == "" {
			cfg.WeComWebhookURL = existing.WeComWebhookURL
		}
	}
	return cfg, scope, nil
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "check local health endpoint")
	flag.Parse()
	if *healthcheck {
		resp, err := http.Get("http://127.0.0.1:8090/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		_ = resp.Body.Close()
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	statePath := envOr("QUOTA_SYNC_STATE_PATH", "/data/state.json")
	baseURL := envOr("SUB2API_BASE_URL", "http://sub2api:8080/api/v1")
	listenAddr := envOr("QUOTA_SYNC_LISTEN", ":8090")
	apiKey, err := readSecret("SUB2API_ADMIN_API_KEY_FILE", "/run/secrets/sub2api_admin_api_key")
	if err != nil {
		logger.Error("read Sub2API admin API key failed", "error", err)
		os.Exit(1)
	}
	store, err := OpenStore(statePath)
	if err != nil {
		logger.Error("open state failed", "error", err)
		os.Exit(1)
	}
	client, err := NewAPIClient(baseURL, apiKey)
	if err != nil {
		logger.Error("create Sub2API client failed", "error", err)
		os.Exit(1)
	}
	restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 15*time.Second)
	restorer, err := NewUsageRestorer(restoreCtx)
	restoreCancel()
	if err != nil {
		logger.Error("initialize rollback storage failed", "error", err)
		os.Exit(1)
	}
	defer restorer.Close()
	notifier := NewNotifier()
	engine := NewEngine(store, client, notifier, restorer, logger)
	server := &Server{store: store, client: client, engine: engine, notifier: notifier, logger: logger}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go engine.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /embed", server.index)
	mux.HandleFunc("GET /menu.js", server.menuScript)
	mux.Handle("GET /api/dashboard", server.adminAuth(http.HandlerFunc(server.dashboard)))
	mux.Handle("POST /api/config", server.adminAuth(http.HandlerFunc(server.updateConfig)))
	mux.Handle("POST /api/poll-now", server.adminAuth(http.HandlerFunc(server.pollNow)))
	mux.Handle("POST /api/bark-test", server.adminAuth(http.HandlerFunc(server.barkTest)))
	mux.Handle("POST /api/wecom-test", server.adminAuth(http.HandlerFunc(server.weComTest)))
	mux.Handle("GET /api/events/{eventID}/targets/{subscriptionID}/undo-preview", server.adminAuth(http.HandlerFunc(server.undoTargetPreview)))
	mux.Handle("POST /api/events/{eventID}/targets/{subscriptionID}/undo", server.adminAuth(http.HandlerFunc(server.undoTarget)))
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		logger.Info("quota sync started", "listen", listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func readSecret(envName, fallback string) (string, error) {
	path := envOr(envName, fallback)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return v, nil
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(authorization, "Bearer ") {
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			err := s.client.ValidateAdminBearer(ctx, authorization)
			cancel()
			if err == nil {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Do not send a Basic challenge for API calls: an expired embedded
		// Sub2API session must not trigger a browser password dialog.
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'self'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// The management UI intentionally has no standalone entry. Modern browsers
	// send Fetch Metadata for iframe navigations; direct tabs and raw links get
	// the same 404 as the sidecar root.
	if r.Header.Get("Sec-Fetch-Dest") != "iframe" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

func (s *Server) menuScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_, _ = io.WriteString(w, menuJS)
}

type dashboardResponse struct {
	State              PersistentState `json:"state"`
	Runtime            EngineRuntime   `json:"runtime"`
	Accounts           []Account       `json:"accounts"`
	Subscriptions      []Subscription  `json:"subscriptions"`
	BarkKeyConfigured  bool            `json:"bark_key_configured"`
	WeComConfigured    bool            `json:"wecom_webhook_configured"`
	BarkKeyMasked      string          `json:"bark_device_key_masked,omitempty"`
	WeComWebhookMasked string          `json:"wecom_webhook_url_masked,omitempty"`
	AccountsError      string          `json:"accounts_error,omitempty"`
	SubsError          string          `json:"subscriptions_error,omitempty"`
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	state := s.store.Snapshot()
	barkConfigured := strings.TrimSpace(state.Config.BarkDeviceKey) != ""
	weComConfigured := strings.TrimSpace(state.Config.WeComWebhookURL) != ""
	barkMasked := maskSecret(state.Config.BarkDeviceKey)
	weComMasked := maskWeComWebhookURL(state.Config.WeComWebhookURL)
	state.Config.BarkDeviceKey = ""
	state.Config.WeComWebhookURL = ""
	resp := dashboardResponse{State: state, Runtime: s.engine.Runtime(), BarkKeyConfigured: barkConfigured, WeComConfigured: weComConfigured, BarkKeyMasked: barkMasked, WeComWebhookMasked: weComMasked}
	var accountErr, subErr error
	done := make(chan struct{}, 2)
	go func() {
		resp.Accounts, accountErr = s.client.ListOAuthAccounts(ctx)
		done <- struct{}{}
	}()
	go func() {
		resp.Subscriptions, subErr = s.client.ListActiveSubscriptions(ctx)
		done <- struct{}{}
	}()
	<-done
	<-done
	if accountErr != nil {
		resp.AccountsError = accountErr.Error()
	}
	if subErr != nil {
		resp.SubsError = subErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	if !validMutation(r) {
		http.Error(w, "invalid mutation request", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req configUpdateRequest
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "配置 JSON 无效: " + err.Error()})
		return
	}
	existing := s.store.Config()
	cfg, scope, err := mergeScopedConfig(existing, req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := validateConfig(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	removedTargets := 0
	if (scope == configScopeAll || scope == configScopeMapping) && len(cfg.Sources) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		type accountResult struct {
			items []Account
			err   error
		}
		type subscriptionResult struct {
			items []Subscription
			err   error
		}
		accountsDone := make(chan accountResult, 1)
		subscriptionsDone := make(chan subscriptionResult, 1)
		go func() {
			items, err := s.client.ListOAuthAccounts(ctx)
			accountsDone <- accountResult{items: items, err: err}
		}()
		go func() {
			items, err := s.client.ListActiveSubscriptions(ctx)
			subscriptionsDone <- subscriptionResult{items: items, err: err}
		}()
		accounts := <-accountsDone
		subscriptions := <-subscriptionsDone
		if accounts.err != nil || subscriptions.err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("读取 Sub2API 账号映射失败: 账号=%v，订阅=%v", accounts.err, subscriptions.err)})
			return
		}
		var err error
		cfg, removedTargets, err = reconcileSourceMappings(cfg, accounts.items, subscriptions.items)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if err := s.store.SetConfig(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.engine.NotifyConfigChanged()
	publicConfig := s.store.Config()
	barkConfigured := publicConfig.BarkDeviceKey != ""
	weComConfigured := publicConfig.WeComWebhookURL != ""
	barkMasked := maskSecret(publicConfig.BarkDeviceKey)
	weComMasked := maskWeComWebhookURL(publicConfig.WeComWebhookURL)
	publicConfig.BarkDeviceKey = ""
	publicConfig.WeComWebhookURL = ""
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": scope, "config": publicConfig, "bark_key_configured": barkConfigured, "wecom_webhook_configured": weComConfigured, "bark_device_key_masked": barkMasked, "wecom_webhook_url_masked": weComMasked, "removed_incompatible_targets": removedTargets})
}

func reconcileSourceMappings(cfg Config, accounts []Account, subscriptions []Subscription) (Config, int, error) {
	out := cfg
	out.Sources = append([]SourceConfig(nil), cfg.Sources...)
	accountByID := make(map[int64]Account, len(accounts))
	for _, account := range accounts {
		accountByID[account.ID] = account
	}
	subscriptionByID := make(map[int64]Subscription, len(subscriptions))
	for _, subscription := range subscriptions {
		if subscription.Status == "" || subscription.Status == "active" {
			subscriptionByID[subscription.ID] = subscription
		}
	}
	removed := 0
	for i := range out.Sources {
		source := &out.Sources[i]
		account, ok := accountByID[source.AccountID]
		if !ok {
			return Config{}, 0, fmt.Errorf("监听账号 #%d 不是可用的 OpenAI OAuth 父账号", source.AccountID)
		}
		source.AccountName = account.Name
		groupIDs := account.mappingGroupIDs()
		clean := make([]int64, 0, len(source.TargetSubscriptionIDs))
		for _, targetID := range source.TargetSubscriptionIDs {
			subscription, exists := subscriptionByID[targetID]
			if !exists {
				removed++
				continue
			}
			if _, allowed := groupIDs[subscription.GroupID]; !allowed {
				removed++
				continue
			}
			clean = append(clean, targetID)
		}
		source.TargetSubscriptionIDs = clean
	}
	return out, removed, nil
}

func (s *Server) pollNow(w http.ResponseWriter, r *http.Request) {
	if !validMutation(r) {
		http.Error(w, "invalid mutation request", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 170*time.Second)
	defer cancel()
	result, err := s.engine.PollNow(ctx)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

func (s *Server) barkTest(w http.ResponseWriter, r *http.Request) {
	if !validMutation(r) {
		http.Error(w, "invalid mutation request", http.StatusForbidden)
		return
	}
	cfg := s.store.Config()
	if cfg.BarkDeviceKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先保存 Bark Device Key"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.notifier.SendBark(ctx, cfg, "Sub2API 自动重置", "Bark 通知测试成功"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) weComTest(w http.ResponseWriter, r *http.Request) {
	if !validMutation(r) {
		http.Error(w, "invalid mutation request", http.StatusForbidden)
		return
	}
	cfg := s.store.Config()
	if cfg.WeComWebhookURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先保存企业微信 Webhook"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.notifier.SendWeCom(ctx, cfg, "Sub2API 自动重置", "企业微信通知测试成功"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type undoUsageValues struct {
	DailyUsageUSD   float64 `json:"daily_usage_usd"`
	WeeklyUsageUSD  float64 `json:"weekly_usage_usd"`
	MonthlyUsageUSD float64 `json:"monthly_usage_usd"`
}

type undoTargetPreviewResponse struct {
	BeforeReset undoUsageValues `json:"before_reset"`
	AfterReset  undoUsageValues `json:"after_reset"`
	AfterUndo   undoUsageValues `json:"after_undo"`
}

func buildUndoTargetPreview(event ResetEvent, target TargetResult, current Subscription, now time.Time) (undoTargetPreviewResponse, error) {
	var preview undoTargetPreviewResponse
	if target.BeforeReset == nil || target.AfterReset == nil {
		return preview, errors.New("该用户缺少重置前后用量记录")
	}
	before, after := *target.BeforeReset, *target.AfterReset
	if current.ID != target.SubscriptionID || current.UserID != before.UserID || current.GroupID != before.GroupID {
		return preview, errors.New("订阅所属用户或分组已改变")
	}
	currentUsage := currentSubscriptionUsage{
		UserID:             current.UserID,
		GroupID:            current.GroupID,
		DailyWindowStart:   current.DailyWindowStart,
		WeeklyWindowStart:  current.WeeklyWindowStart,
		MonthlyWindowStart: current.MonthlyWindowStart,
		DailyUsageUSD:      current.DailyUsageUSD,
		WeeklyUsageUSD:     current.WeeklyUsageUSD,
		MonthlyUsageUSD:    current.MonthlyUsageUSD,
	}
	plan, err := resolveRestoreWindows(currentUsage, before, after, event.ResetDaily, event.ResetWeekly, event.ResetMonthly, now)
	if err != nil {
		return preview, err
	}
	preview.BeforeReset = undoUsageValues{DailyUsageUSD: before.DailyUsageUSD, WeeklyUsageUSD: before.WeeklyUsageUSD, MonthlyUsageUSD: before.MonthlyUsageUSD}
	preview.AfterReset = undoUsageValues{DailyUsageUSD: current.DailyUsageUSD, WeeklyUsageUSD: current.WeeklyUsageUSD, MonthlyUsageUSD: current.MonthlyUsageUSD}
	preview.AfterUndo = preview.AfterReset
	if plan.Daily {
		preview.AfterUndo.DailyUsageUSD += before.DailyUsageUSD
	}
	if plan.Weekly {
		preview.AfterUndo.WeeklyUsageUSD += before.WeeklyUsageUSD
	}
	if plan.Monthly {
		preview.AfterUndo.MonthlyUsageUSD += before.MonthlyUsageUSD
	}
	return preview, nil
}

func (s *Server) undoTargetPreview(w http.ResponseWriter, r *http.Request) {
	eventID := strings.TrimSpace(r.PathValue("eventID"))
	if eventID == "" || len(eventID) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "重置记录编号无效"})
		return
	}
	subscriptionID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("subscriptionID")), 10, 64)
	if err != nil || subscriptionID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "下游订阅编号无效"})
		return
	}
	event, ok := s.store.Event(eventID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": errUndoEventNotFound.Error()})
		return
	}
	var target *TargetResult
	for i := range event.Targets {
		if event.Targets[i].SubscriptionID == subscriptionID {
			target = &event.Targets[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": errUndoTargetNotFound.Error()})
		return
	}
	if target.Status != "succeeded" || target.UndoStatus == "succeeded" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该用户当前不能撤销"})
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, event.UndoExpiresAt)
	if err != nil || !time.Now().Before(expiresAt) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该用户的重置记录已超过 24 小时撤销时限"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	current, err := s.client.GetSubscription(ctx, subscriptionID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "读取该用户最新用量失败: " + err.Error()})
		return
	}
	preview, err := buildUndoTargetPreview(event, *target, current, time.Now())
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preview": preview})
}

func (s *Server) undoTarget(w http.ResponseWriter, r *http.Request) {
	if !validMutation(r) {
		http.Error(w, "invalid mutation request", http.StatusForbidden)
		return
	}
	eventID := strings.TrimSpace(r.PathValue("eventID"))
	if eventID == "" || len(eventID) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "重置记录编号无效"})
		return
	}
	subscriptionID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("subscriptionID")), 10, 64)
	if err != nil || subscriptionID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "下游订阅编号无效"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	result, err := s.engine.UndoTarget(ctx, eventID, subscriptionID)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, errUndoEventNotFound) || errors.Is(err, errUndoTargetNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "result": result})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

func validMutation(r *http.Request) bool {
	return r.Header.Get("X-Quota-Sync-CSRF") == "1" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
