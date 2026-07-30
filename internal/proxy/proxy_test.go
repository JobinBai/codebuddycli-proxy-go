package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHeadersForAPIKey(t *testing.T) {
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", DefaultModel: "hy3", SessionTTL: time.Minute}, "test", io.Discard)
	s := newSession()
	headers := h.headers(s)
	if headers["Authorization"] != "Bearer ck_example_12345678" || headers["X-API-Key"] == "" {
		t.Fatal("missing API key headers")
	}
	if headers["X-User-Id"] != "anonymous_12345678" {
		t.Fatalf("unexpected user id: %s", headers["X-User-Id"])
	}
	if headers["X-Request-ID"] != headers["X-Conversation-Message-ID"] {
		t.Fatal("request ids must match")
	}
}

func TestNonStreamingRequestIsAggregated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("upstream stream=%v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute}, "test", io.Discard)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	choices := out["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("content=%v", message["content"])
	}
}

func TestTextLogsIncludePromptAndUsageWithoutAPIKey(t *testing.T) {
	const apiKey = "ck_secret_key_must_not_be_logged"
	var logs bytes.Buffer
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: apiKey, BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute}, "test", &logs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"private prompt"}],"stream":false}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	got := logs.String()
	if !bytes.Contains([]byte(got), []byte("INFO  [codebuddy-proxy] chat request completed")) {
		t.Fatalf("completion event missing from logs: %s", got)
	}
	for _, expected := range []string{"private prompt", "prompt_tokens=5", "completion_tokens=2", "total_tokens=7", "cached_tokens=3"} {
		if !bytes.Contains([]byte(got), []byte(expected)) {
			t.Fatalf("logs omit %q: %s", expected, got)
		}
	}
	if bytes.Contains([]byte(got), []byte(apiKey)) {
		t.Fatalf("logs expose API key: %s", got)
	}
}
