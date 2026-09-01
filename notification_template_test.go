package main

import (
	"strings"
	"testing"
)

func TestDefaultNotificationTemplatesValidateAndRender(t *testing.T) {
	cfg := defaultConfig()
	if err := validateNotificationTemplates(cfg); err != nil {
		t.Fatalf("default templates must validate: %v", err)
	}
	event := ResetEvent{
		ID:                "evt-1",
		SourceAccountID:   42,
		SourceAccountName: "OAuth 主账号",
		PreviousUsage:     UsageTotals{Requests: 170, Tokens: 14_800_000, Cost: 7.32},
		ConfirmedUsage:    UsageTotals{Requests: 12, Tokens: 900_000, Cost: 0.48},
		PreviousCredits:   2,
		ConfirmedCredits:  2,
	}
	content := buildNotificationContent(cfg, "detection", event, 0, 0)
	for _, expected := range []string{"OAuth 主账号", "170 req", "14.80M tokens", "12 req", "重置卡：2 → 2"} {
		if !strings.Contains(content.Body, expected) {
			t.Fatalf("rendered body %q is missing %q", content.Body, expected)
		}
	}
}

func TestCustomNotificationTemplateRendersVariables(t *testing.T) {
	cfg := defaultConfig()
	cfg.ResetNotificationTitle = "{{source_name}} / {{event_id}}"
	cfg.ResetNotificationBody = "{{previous_requests}} → {{confirmed_requests}}；{{succeeded}} 成功；{{failed}} 失败；{{reset_dimensions}}"
	if err := validateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	event := ResetEvent{
		ID:                "evt-custom",
		SourceAccountID:   7,
		SourceAccountName: "OAuth A",
		PreviousUsage:     UsageTotals{Requests: 88},
		ConfirmedUsage:    UsageTotals{Requests: 3},
		ResetDaily:        true,
		ResetWeekly:       true,
	}
	content := buildNotificationContent(cfg, "reset", event, 4, 1)
	if content.Title != "OAuth A / evt-custom" || content.Body != "88 → 3；4 成功；1 失败；日、周" {
		t.Fatalf("unexpected rendered notification: %+v", content)
	}
}

func TestDefaultUndoNotificationIncludesRolledBackAccount(t *testing.T) {
	cfg := defaultConfig()
	event := ResetEvent{SourceAccountID: 7, SourceAccountName: "OAuth A"}
	target := TargetResult{
		SubscriptionID: 3,
		BeforeReset: &SubscriptionUsageSnapshot{
			SubscriptionID: 3,
			UserID:         18,
			UserEmail:      "user@example.com",
			GroupName:      "默认分组",
		},
	}
	content := buildTargetNotificationContent(cfg, "undo", event, 1, 0, &target)
	if !strings.Contains(content.Body, "下游账号：user@example.com") {
		t.Fatalf("undo notification omitted downstream account: %q", content.Body)
	}
}

func TestLegacyDefaultUndoNotificationUpgradesToAccountTemplate(t *testing.T) {
	cfg := defaultConfig()
	cfg.UndoNotificationBody = legacyDefaultUndoNotificationBody
	applyConfigDefaults(&cfg)
	if cfg.UndoNotificationBody != defaultUndoNotificationBody {
		t.Fatalf("legacy default was not upgraded: %q", cfg.UndoNotificationBody)
	}
}

func TestNotificationTemplateRejectsUnknownVariable(t *testing.T) {
	cfg := defaultConfig()
	cfg.UndoNotificationBody = "{{unknown_value}}"
	if err := validateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "不支持的变量") {
		t.Fatalf("expected unknown variable error, got %v", err)
	}
}

func TestNotificationSecretMasksKeepEdges(t *testing.T) {
	if got := maskSecret("abcdefghijkl"); got != "abcd********ijkl" {
		t.Fatalf("unexpected secret mask: %q", got)
	}
	if got := maskSecret("abcdef"); got != "a********f" {
		t.Fatalf("unexpected short secret mask: %q", got)
	}
	want := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=secr********-key"
	if got := maskWeComWebhookURL("https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=secret-key"); got != want {
		t.Fatalf("unexpected webhook mask: %q", got)
	}
}
