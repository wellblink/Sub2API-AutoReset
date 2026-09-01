package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	defaultDetectionNotificationTitle = "Sub2API 检测到上游重置"
	defaultDetectionNotificationBody  = "上游账号：{{source_name}}\n7d 用量：{{previous_usage}} → {{confirmed_usage}}\n重置卡：{{previous_credits}} → {{confirmed_credits}}"
	defaultResetNotificationTitle     = "Sub2API 下游配额重置完成"
	defaultResetNotificationBody      = "上游账号：{{source_name}}\n7d 用量：{{previous_usage}} → {{confirmed_usage}}\n下游结果：{{succeeded}} 成功，{{failed}} 失败"
	defaultUndoNotificationTitle      = "Sub2API 下游配额重置已撤销"
	legacyDefaultUndoNotificationBody = "上游账号：{{source_name}}\n撤销结果：{{succeeded}} 成功，{{failed}} 失败"
	defaultUndoNotificationBody       = "上游账号：{{source_name}}\n下游账号：{{target_account}}\n撤销结果：{{succeeded}} 成功，{{failed}} 失败"
)

var (
	notificationPlaceholder = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)
	notificationVariables   = map[string]struct{}{
		"source_name": {}, "source_id": {}, "event_id": {}, "detected_at": {},
		"previous_usage": {}, "confirmed_usage": {}, "previous_requests": {}, "confirmed_requests": {},
		"previous_tokens": {}, "confirmed_tokens": {}, "previous_cost": {}, "confirmed_cost": {},
		"previous_standard_cost": {}, "confirmed_standard_cost": {}, "previous_user_cost": {}, "confirmed_user_cost": {},
		"previous_credits": {}, "confirmed_credits": {}, "succeeded": {}, "failed": {}, "reset_dimensions": {},
		"target_account": {}, "target_group": {}, "target_subscription_id": {},
	}
)

type NotificationContent struct {
	Title string
	Body  string
}

func normalizeNotificationTemplates(c *Config) {
	c.DetectionNotificationTitle = strings.TrimSpace(c.DetectionNotificationTitle)
	c.DetectionNotificationBody = strings.TrimSpace(c.DetectionNotificationBody)
	c.ResetNotificationTitle = strings.TrimSpace(c.ResetNotificationTitle)
	c.ResetNotificationBody = strings.TrimSpace(c.ResetNotificationBody)
	c.UndoNotificationTitle = strings.TrimSpace(c.UndoNotificationTitle)
	c.UndoNotificationBody = strings.TrimSpace(c.UndoNotificationBody)
}

func validateNotificationTemplates(c Config) error {
	fields := []struct {
		name  string
		value string
		max   int
	}{
		{"上游检测通知标题", c.DetectionNotificationTitle, 120},
		{"上游检测通知内容", c.DetectionNotificationBody, 2000},
		{"下游重置通知标题", c.ResetNotificationTitle, 120},
		{"下游重置通知内容", c.ResetNotificationBody, 2000},
		{"撤销通知标题", c.UndoNotificationTitle, 120},
		{"撤销通知内容", c.UndoNotificationBody, 2000},
	}
	for _, field := range fields {
		if field.value == "" {
			return fmt.Errorf("%s不能为空", field.name)
		}
		if utf8.RuneCountInString(field.value) > field.max {
			return fmt.Errorf("%s不能超过 %d 个字符", field.name, field.max)
		}
		for _, match := range notificationPlaceholder.FindAllStringSubmatch(field.value, -1) {
			if _, ok := notificationVariables[match[1]]; !ok {
				return fmt.Errorf("%s包含不支持的变量 {{%s}}", field.name, match[1])
			}
		}
		withoutKnown := notificationPlaceholder.ReplaceAllString(field.value, "")
		if strings.Contains(withoutKnown, "{{") || strings.Contains(withoutKnown, "}}") {
			return fmt.Errorf("%s包含无效变量格式", field.name)
		}
	}
	return nil
}

func renderNotificationTemplate(raw string, values map[string]string) string {
	return notificationPlaceholder.ReplaceAllStringFunc(raw, func(token string) string {
		match := notificationPlaceholder.FindStringSubmatch(token)
		if len(match) != 2 {
			return token
		}
		return values[match[1]]
	})
}

func buildNotificationContent(cfg Config, phase string, event ResetEvent, succeeded, failed int) NotificationContent {
	return buildTargetNotificationContent(cfg, phase, event, succeeded, failed, nil)
}

func buildTargetNotificationContent(cfg Config, phase string, event ResetEvent, succeeded, failed int, target *TargetResult) NotificationContent {
	values := notificationTemplateValues(event, succeeded, failed, target)
	title, body := cfg.DetectionNotificationTitle, cfg.DetectionNotificationBody
	switch phase {
	case "reset":
		title, body = cfg.ResetNotificationTitle, cfg.ResetNotificationBody
	case "undo":
		title, body = cfg.UndoNotificationTitle, cfg.UndoNotificationBody
	}
	return NotificationContent{
		Title: renderNotificationTemplate(title, values),
		Body:  renderNotificationTemplate(body, values),
	}
}

func notificationTemplateValues(event ResetEvent, succeeded, failed int, target *TargetResult) map[string]string {
	sourceName := strings.TrimSpace(event.SourceAccountName)
	if sourceName == "" {
		sourceName = fmt.Sprintf("账号 #%d", event.SourceAccountID)
	}
	dimensions := make([]string, 0, 3)
	if event.ResetDaily {
		dimensions = append(dimensions, "日")
	}
	if event.ResetWeekly {
		dimensions = append(dimensions, "周")
	}
	if event.ResetMonthly {
		dimensions = append(dimensions, "月")
	}
	values := map[string]string{
		"source_name": sourceName, "source_id": strconv.FormatInt(event.SourceAccountID, 10),
		"event_id": event.ID, "detected_at": event.DetectedAt,
		"previous_usage": formatNotificationUsage(event.PreviousUsage), "confirmed_usage": formatNotificationUsage(event.ConfirmedUsage),
		"previous_requests": strconv.FormatInt(event.PreviousUsage.Requests, 10), "confirmed_requests": strconv.FormatInt(event.ConfirmedUsage.Requests, 10),
		"previous_tokens": strconv.FormatInt(event.PreviousUsage.Tokens, 10), "confirmed_tokens": strconv.FormatInt(event.ConfirmedUsage.Tokens, 10),
		"previous_cost": fmt.Sprintf("%.2f", event.PreviousUsage.Cost), "confirmed_cost": fmt.Sprintf("%.2f", event.ConfirmedUsage.Cost),
		"previous_standard_cost": fmt.Sprintf("%.2f", event.PreviousUsage.StandardCost), "confirmed_standard_cost": fmt.Sprintf("%.2f", event.ConfirmedUsage.StandardCost),
		"previous_user_cost": fmt.Sprintf("%.2f", event.PreviousUsage.UserCost), "confirmed_user_cost": fmt.Sprintf("%.2f", event.ConfirmedUsage.UserCost),
		"previous_credits": strconv.Itoa(event.PreviousCredits), "confirmed_credits": strconv.Itoa(event.ConfirmedCredits),
		"succeeded": strconv.Itoa(succeeded), "failed": strconv.Itoa(failed), "reset_dimensions": strings.Join(dimensions, "、"),
		"target_account": "", "target_group": "", "target_subscription_id": "",
	}
	if target == nil {
		return values
	}
	values["target_subscription_id"] = strconv.FormatInt(target.SubscriptionID, 10)
	snapshot := target.BeforeReset
	if snapshot == nil {
		snapshot = target.AfterReset
	}
	if snapshot == nil {
		values["target_account"] = fmt.Sprintf("订阅 #%d", target.SubscriptionID)
		return values
	}
	values["target_group"] = strings.TrimSpace(snapshot.GroupName)
	values["target_account"] = strings.TrimSpace(snapshot.UserEmail)
	if values["target_account"] == "" {
		values["target_account"] = strings.TrimSpace(snapshot.Username)
	}
	if values["target_account"] == "" {
		values["target_account"] = fmt.Sprintf("用户 #%d", snapshot.UserID)
	}
	return values
}

func formatNotificationUsage(usage UsageTotals) string {
	return fmt.Sprintf("%d req / %.2fM tokens / A $%.2f", usage.Requests, float64(usage.Tokens)/1_000_000, usage.Cost)
}

func maskSecret(raw string) string {
	runes := []rune(strings.TrimSpace(raw))
	if len(runes) == 0 {
		return ""
	}
	prefix, suffix := 4, 4
	if len(runes) < 9 {
		prefix, suffix = 1, 1
	}
	return string(runes[:prefix]) + "********" + string(runes[len(runes)-suffix:])
}

func maskWeComWebhookURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return maskSecret(raw)
	}
	key := u.Query().Get("key")
	if key == "" {
		return maskSecret(raw)
	}
	u.RawQuery = "key=" + maskSecret(key)
	u.Fragment = ""
	u.User = nil
	return u.String()
}
