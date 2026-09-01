package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbedOnlyAllowsIframeNavigation(t *testing.T) {
	server := &Server{}
	direct := httptest.NewRequest(http.MethodGet, "/embed", nil)
	directRecorder := httptest.NewRecorder()
	server.index(directRecorder, direct)
	if directRecorder.Code != http.StatusNotFound {
		t.Fatalf("direct navigation must be hidden, got %d", directRecorder.Code)
	}

	iframe := httptest.NewRequest(http.MethodGet, "/embed", nil)
	iframe.Header.Set("Sec-Fetch-Dest", "iframe")
	iframeRecorder := httptest.NewRecorder()
	server.index(iframeRecorder, iframe)
	if iframeRecorder.Code != http.StatusOK {
		t.Fatalf("iframe navigation must be allowed, got %d", iframeRecorder.Code)
	}
}

func TestAdminAuthAcceptsValidatedSub2APIBearer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer valid-admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"id":1,"role":"admin"}}`))
	}))
	defer upstream.Close()

	client, err := NewAPIClient(upstream.URL, "service-api-key")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{client: client}
	handler := server.adminAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	req.Header.Set("Authorization", "Bearer valid-admin")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected bearer auth to pass, got %d", recorder.Code)
	}
}

func TestAdminAuthRejectsInvalidBearerWithoutBasicChallenge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"unauthorized"}`))
	}))
	defer upstream.Close()

	client, err := NewAPIClient(upstream.URL, "service-api-key")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{client: client}
	handler := server.adminAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected bearer auth to fail, got %d", recorder.Code)
	}
	if recorder.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("embedded API failure must not trigger a Basic auth dialog")
	}
}

func TestEmbeddedUIPlacesUndoOnIndividualUsers(t *testing.T) {
	for _, required := range []string{
		"通知设置",
		"企业微信",
		"通知内容",
		"下游账号：{{target_account}}",
		`value="{{target_account}}">下游账号`,
		"每个重置成功的用户可在 24 小时内独立撤销",
		"成功 ${Number(dashboard.state.poll_succeeded||0)} 次，失败 ${Number(dashboard.state.poll_failed||0)} 次",
		"最近采样与自然重置时间",
		`class="btn small danger target-undo"`,
		`data-subscription-id="${Number(t.subscription_id)}"`,
		"确认撤销此用户用量",
		"重置后用量",
		"撤销后用量",
		"该用户额度重置已回滚",
		"/undo-preview",
		"/targets/${encodeURIComponent(subscriptionID)}/undo",
		`id="saveFooter"`,
		"activeView='overview'",
		"resetView(activeView);activeView=next;resetView(activeView)",
		"function buildConfig(scope=activeView)",
		"saveConfig({scope:activeView})",
		"saveConfig({scope:'notifications',silent:true})",
		"r.result?.message",
		"groupIDsForAccount",
		"待保存",
		"eventTargetLabel",
		"重置前：",
		"分组未知",
		"bark_device_key_masked",
		"top:max(16px,env(safe-area-inset-top))",
	} {
		if !strings.Contains(indexHTML, required) {
			t.Fatalf("embedded UI is missing %q", required)
		}
	}
	for _, removed := range []string{
		"检测直接使用“查询”返回的 req",
		"检查已启动；首次采样只会建立基线",
		"undo_enabled",
		"undo_window_minutes",
		"撤销保护开关",
		"selected?statusBadge('监听中'",
		"最近完成",
		".toast-stack{position:fixed;right:16px;bottom:",
		`id="barkKey" type="password"`,
		`id="weComWebhook" type="password"`,
		"撤销最近重置",
		`id="undoBox"`,
		`id="undoBtn"`,
		"/api/undo-last",
		"function latestResetEvent",
		"renderUndo()",
		`id="refreshBtn"`,
		` · 事件 ${esc(e.id)}`,
		`事件编号：${e.id}`,
		`class="btn small danger event-undo"`,
		"确认撤销此条重置记录",
		"待撤销下游",
		"/api/events/${encodeURIComponent(eventID)}/undo",
		"function pendingUndoTargets",
		"function undoEvent",
		"statusBadge(label,kind)",
		"个可映射订阅",
		`id="subCount"`,
		"r.running?'运行中':'空闲'",
		"最近采样、自然重置时间与判定结果",
		"<th>最近判定</th>",
		"decisions[s.last_decision]",
		"该用户用量已撤销",
		"只恢复这一名用户在本次重置前的用量",
		"不会撤销同一记录里的其他用户",
		"若订阅窗口后来再次变化",
	} {
		if strings.Contains(indexHTML, removed) {
			t.Fatalf("embedded UI still contains removed content %q", removed)
		}
	}
}

func TestUpdateConfigScopesPreserveOtherTabs(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Config()
	cfg.Enabled = true
	cfg.PollIntervalSeconds = 120
	cfg.BarkGroup = "保留的通知分组"
	cfg.BarkDeviceKey = "saved-bark-key"
	cfg.Sources = []SourceConfig{{
		AccountID:             9,
		AccountName:           "saved-source",
		Enabled:               true,
		TargetSubscriptionIDs: []int64{19},
		ResetWeekly:           true,
	}}
	if err := store.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, engine: &Engine{wake: make(chan struct{}, 1)}}
	post := func(payload string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Quota-Sync-CSRF", "1")
		recorder := httptest.NewRecorder()
		server.updateConfig(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("unexpected response %d: %s", recorder.Code, recorder.Body.String())
		}
	}

	post(`{"scope":"overview","enabled":false,"poll_interval_seconds":240,"confirm_delay_seconds":12,"natural_grace_seconds":180,"max_sample_age_seconds":360}`)
	afterOverview := store.Config()
	if afterOverview.Enabled || afterOverview.PollIntervalSeconds != 240 {
		t.Fatalf("overview fields were not saved: %+v", afterOverview)
	}
	if afterOverview.BarkGroup != cfg.BarkGroup || afterOverview.BarkDeviceKey != cfg.BarkDeviceKey || len(afterOverview.Sources) != 1 || afterOverview.Sources[0].AccountID != 9 {
		t.Fatal("overview save overwrote notification or mapping settings")
	}

	notificationConfig := defaultConfig()
	notificationConfig.BarkGroup = "新的通知分组"
	notificationPayload, err := json.Marshal(configUpdateRequest{Scope: configScopeNotifications, Config: notificationConfig})
	if err != nil {
		t.Fatal(err)
	}
	post(string(notificationPayload))
	afterNotifications := store.Config()
	if afterNotifications.BarkGroup != notificationConfig.BarkGroup || afterNotifications.BarkDeviceKey != cfg.BarkDeviceKey {
		t.Fatal("notification scope did not save notification settings or preserve its secret")
	}
	if afterNotifications.Enabled || afterNotifications.PollIntervalSeconds != 240 || len(afterNotifications.Sources) != 1 || afterNotifications.Sources[0].AccountID != 9 {
		t.Fatal("notification save overwrote overview or mapping settings")
	}

	post(`{"scope":"mapping","sources":[]}`)
	afterMapping := store.Config()
	if len(afterMapping.Sources) != 0 {
		t.Fatal("mapping scope did not replace the saved mappings")
	}
	if afterMapping.Enabled || afterMapping.PollIntervalSeconds != 240 || afterMapping.BarkGroup != notificationConfig.BarkGroup || afterMapping.BarkDeviceKey != cfg.BarkDeviceKey {
		t.Fatal("mapping save overwrote overview or notification settings")
	}
}

func TestUpdateConfigPreservesAndMasksNotificationSecrets(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Config()
	cfg.BarkEnabled = true
	cfg.BarkDeviceKey = "bark-super-secret-key"
	cfg.WeComEnabled = true
	cfg.WeComWebhookURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=secret-key"
	if err := store.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	publicCfg := cfg
	publicCfg.BarkDeviceKey = ""
	publicCfg.WeComWebhookURL = ""
	payload, err := json.Marshal(publicCfg)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, engine: &Engine{wake: make(chan struct{}, 1)}}
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Quota-Sync-CSRF", "1")
	recorder := httptest.NewRecorder()
	server.updateConfig(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected response %d: %s", recorder.Code, recorder.Body.String())
	}
	if store.Config().WeComWebhookURL != cfg.WeComWebhookURL {
		t.Fatal("blank public webhook value did not preserve the stored secret")
	}
	if store.Config().BarkDeviceKey != cfg.BarkDeviceKey {
		t.Fatal("blank public Bark key did not preserve the stored secret")
	}
	responseBody := recorder.Body.String()
	if strings.Contains(responseBody, "bark-super-secret-key") || strings.Contains(responseBody, "key=secret-key") ||
		!strings.Contains(responseBody, `"bark_key_configured":true`) || !strings.Contains(responseBody, `"wecom_webhook_configured":true`) ||
		!strings.Contains(responseBody, `"bark_device_key_masked":"bark********-key"`) ||
		!strings.Contains(responseBody, `"wecom_webhook_url_masked":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=secr********-key"`) {
		t.Fatalf("response exposed or lost webhook state: %s", recorder.Body.String())
	}
}
