package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNotifierUsesBarkV2JSONEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/push" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["device_key"] != "secret-key" || body["title"] != "title" || body["body"] != "body" {
			t.Fatalf("unexpected Bark payload: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success"})
	}))
	defer server.Close()
	notifier := NewNotifier()
	cfg := defaultConfig()
	cfg.BarkEnabled = true
	cfg.BarkServerURL = server.URL
	cfg.BarkDeviceKey = "secret-key"
	if err := notifier.SendBark(context.Background(), cfg, "title", "body"); err != nil {
		t.Fatal(err)
	}
}

func TestNotifierUsesWeComTextWebhook(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/cgi-bin/webhook/send" || r.URL.Query().Get("key") != "secret-key" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		var body struct {
			MessageType string `json:"msgtype"`
			Text        struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.MessageType != "text" || body.Text.Content != "title\nbody" {
			t.Fatalf("unexpected WeCom payload: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "errmsg": "ok"})
	}))
	defer server.Close()
	notifier := NewNotifier()
	cfg := defaultConfig()
	cfg.WeComEnabled = true
	cfg.WeComWebhookURL = server.URL + "/cgi-bin/webhook/send?key=secret-key"
	if err := notifier.SendWeCom(context.Background(), cfg, "title", "body"); err != nil {
		t.Fatal(err)
	}
}

func TestWeComTextContentUsesUTF8ByteLimit(t *testing.T) {
	content := truncateUTF8Bytes(strings.Repeat("中", 1000), 2048)
	if len(content) > 2048 {
		t.Fatalf("content exceeds WeCom text limit: %d bytes", len(content))
	}
	if !utf8.ValidString(content) {
		t.Fatal("content was truncated inside a UTF-8 rune")
	}
	if len(content) != 2046 {
		t.Fatalf("expected the largest valid prefix, got %d bytes", len(content))
	}
}

func TestSendConfiguredDeliversBothChannels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/push" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "errmsg": "ok"})
	}))
	defer server.Close()
	notifier := NewNotifier()
	cfg := defaultConfig()
	cfg.BarkEnabled = true
	cfg.BarkServerURL = server.URL
	cfg.BarkDeviceKey = "bark-key"
	cfg.WeComEnabled = true
	cfg.WeComWebhookURL = server.URL + "/cgi-bin/webhook/send?key=wecom-key"
	result := notifier.SendConfigured(context.Background(), cfg, "title", "body")
	if result.Attempted != 2 || result.Succeeded != 2 || len(result.Errors) != 0 {
		t.Fatalf("unexpected delivery result: %+v", result)
	}
}

func TestValidateBarkURLRejectsCredentials(t *testing.T) {
	if err := validateBarkURL("https://user:pass@api.day.app"); err == nil {
		t.Fatal("expected credential-bearing URL to be rejected")
	}
}

func TestValidateWeComURLRequiresWebhookKey(t *testing.T) {
	if err := validateWeComWebhookURL("https://qyapi.weixin.qq.com/cgi-bin/webhook/send"); err == nil {
		t.Fatal("expected webhook without key to be rejected")
	}
}
