package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Notifier struct {
	http *http.Client
}

type NotificationSendResult struct {
	Attempted int
	Succeeded int
	Errors    []string
}

func NewNotifier() *Notifier {
	return &Notifier{http: &http.Client{Timeout: 15 * time.Second}}
}

func validateBarkURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("Bark 服务地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("Bark 服务地址必须是有效的 HTTP 或 HTTPS 地址")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("Bark 服务地址不能包含账号、查询参数或锚点")
	}
	if len(raw) > 2048 {
		return errors.New("Bark 服务地址过长")
	}
	return nil
}

func validateWeComWebhookURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("企业微信 Webhook 地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("企业微信 Webhook 必须是有效的 HTTP 或 HTTPS 地址")
	}
	if u.User != nil || u.Fragment != "" {
		return errors.New("企业微信 Webhook 不能包含账号信息或锚点")
	}
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/cgi-bin/webhook/send") || strings.TrimSpace(u.Query().Get("key")) == "" {
		return errors.New("企业微信 Webhook 地址格式无效")
	}
	if len(raw) > 4096 {
		return errors.New("企业微信 Webhook 地址过长")
	}
	return nil
}

func (n *Notifier) SendConfigured(ctx context.Context, cfg Config, title, body string) NotificationSendResult {
	type delivery struct {
		channel string
		err     error
	}
	jobs := make([]func() delivery, 0, 2)
	if cfg.BarkEnabled {
		jobs = append(jobs, func() delivery { return delivery{channel: "Bark", err: n.SendBark(ctx, cfg, title, body)} })
	}
	if cfg.WeComEnabled {
		jobs = append(jobs, func() delivery { return delivery{channel: "企业微信", err: n.SendWeCom(ctx, cfg, title, body)} })
	}
	result := NotificationSendResult{Attempted: len(jobs)}
	if len(jobs) == 0 {
		return result
	}
	results := make(chan delivery, len(jobs))
	var wg sync.WaitGroup
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- job()
		}()
	}
	wg.Wait()
	close(results)
	for delivery := range results {
		if delivery.err == nil {
			result.Succeeded++
			continue
		}
		result.Errors = append(result.Errors, delivery.channel+"："+delivery.err.Error())
	}
	return result
}

func (n *Notifier) SendBark(ctx context.Context, cfg Config, title, body string) error {
	if err := validateBarkURL(cfg.BarkServerURL); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.BarkDeviceKey) == "" {
		return errors.New("Device Key 未配置")
	}
	endpoint := strings.TrimRight(cfg.BarkServerURL, "/")
	if !strings.HasSuffix(endpoint, "/push") {
		endpoint += "/push"
	}
	payload := map[string]any{
		"device_key": cfg.BarkDeviceKey,
		"title":      title,
		"body":       body,
		"group":      cfg.BarkGroup,
		"level":      cfg.BarkLevel,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "sub2api-quota-sync/3.0")
	resp, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	responseData, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return errors.New("读取响应失败")
	}
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(responseData, &result); err != nil {
		return fmt.Errorf("返回了无效响应 (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || (result.Code != 0 && result.Code != 200) {
		message := strings.TrimSpace(result.Message)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("推送失败: %s", message)
	}
	return nil
}

func (n *Notifier) SendWeCom(ctx context.Context, cfg Config, title, body string) error {
	if err := validateWeComWebhookURL(cfg.WeComWebhookURL); err != nil {
		return err
	}
	content := truncateUTF8Bytes(title+"\n"+body, 2048)
	payload := map[string]any{
		"msgtype": "text",
		"text": map[string]string{
			"content": content,
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WeComWebhookURL, bytes.NewReader(data))
	if err != nil {
		return errors.New("创建请求失败")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "sub2api-quota-sync/3.0")
	resp, err := n.http.Do(req)
	if err != nil {
		// A url.Error contains the full webhook, including its secret key.
		return errors.New("请求失败")
	}
	defer resp.Body.Close()
	responseData, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return errors.New("读取响应失败")
	}
	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(responseData, &result); err != nil {
		return fmt.Errorf("返回了无效响应 (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.ErrCode != 0 {
		message := strings.TrimSpace(result.ErrMsg)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("推送失败: %s", message)
	}
	return nil
}

func truncateUTF8Bytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}
