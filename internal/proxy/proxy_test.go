package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestStreamingReasoningIsHiddenWhenDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"We need analyze the image\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"图片中是红色。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute}, "test", io.Discard)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"analyze image"}],"stream":true}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()
	if strings.Contains(got, "reasoning_content") {
		t.Fatalf("reasoning chunks must be hidden by default: %s", got)
	}
	if !strings.Contains(got, "图片中是红色。") {
		t.Fatalf("final content missing: %s", got)
	}
	if events := strings.Count(got, "data: "); events != 2 { // final answer + [DONE]
		t.Fatalf("expected hidden reasoning event to be omitted, got %d events: %s", events, got)
	}
}

func TestReasoningIsShownByDefault(t *testing.T) {
	t.Setenv("CODEBUDDY_API_KEY", "ck_example_12345678")
	t.Setenv("HIDE_REASONING", "")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ShowReasoning {
		t.Fatal("reasoning must be shown when HIDE_REASONING is not set")
	}
	t.Setenv("HIDE_REASONING", "1")
	cfg, err = LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ShowReasoning {
		t.Fatal("reasoning must be hidden when HIDE_REASONING=1")
	}
}

func TestStreamingReasoningCanBeEnabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ShowReasoning: true}, "test", io.Discard)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if !strings.Contains(rr.Body.String(), "reasoning_content") {
		t.Fatalf("reasoning chunks must be retained when explicitly enabled: %s", rr.Body.String())
	}
}

func TestImageStreamingReasoningIsAggregated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"We need \"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"analyze the image.\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"图片中是红色。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ShowReasoning: true}, "test", io.Discard)
	payload := `{"messages":[{"role":"user","content":[{"type":"text","text":"analyze image"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"stream":true}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()
	if chunks := strings.Count(got, "reasoning_content"); chunks != 1 {
		t.Fatalf("expected one aggregated reasoning event, got %d: %s", chunks, got)
	}
	if !strings.Contains(got, "We need analyze the image.") {
		t.Fatalf("aggregated reasoning missing: %s", got)
	}
	if strings.Index(got, "reasoning_content") > strings.Index(got, "图片中是红色。") {
		t.Fatalf("reasoning must precede final content: %s", got)
	}
}

func TestModelsRefreshFromRemoteConfiguration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/config" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("User-Agent") != "CodeBuddy/"+cliVersion {
			t.Fatalf("unexpected user agent %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("X-Product-Version") != cliVersion {
			t.Fatalf("unexpected product version %q", r.Header.Get("X-Product-Version"))
		}
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"models": []any{
			map[string]any{"id": "vision-model"}, map[string]any{"id": "disabled-model", "disabled": true}, map[string]any{"id": "vision-model"},
		}}})
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL + "/v2", DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ModelRefreshTTL: time.Hour}, "test", io.Discard)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("X-CodeBuddy-Models-Source") != "remote" {
		t.Fatalf("status=%d source=%q", rr.Code, rr.Header().Get("X-CodeBuddy-Models-Source"))
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0].ID != "vision-model" {
		t.Fatalf("unexpected models: %#v", body.Data)
	}
}

func TestModelsCacheFallbackAfterFailedRefresh(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL + "/v2", DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ModelRefreshTTL: time.Hour}, "test", io.Discard)
	for range 2 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if rr.Code != http.StatusOK || rr.Header().Get("X-CodeBuddy-Models-Source") != "fallback" {
			t.Fatalf("status=%d source=%q", rr.Code, rr.Header().Get("X-CodeBuddy-Models-Source"))
		}
	}
	if requests != 1 {
		t.Fatalf("expected one refresh attempt, got %d", requests)
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

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestTextStreamingReasoningIsCoalesced verifies that a text request with
// reasoning enabled coalesces the many upstream reasoning_content deltas into a
// small number of events (bounded buffering) while keeping every fragment and
// preserving reasoning-before-answer ordering. This is the elegant middle path
// between full aggregate (bad first-token latency) and 1:1 passthrough (hundreds
// of cards in clients that render one card per delta).
func TestTextStreamingReasoningIsCoalesced(t *testing.T) {
	fragments := []string{"思", "考", "：", "农", "夫", "有", "17", "只", "羊", "，", "剩", "9只"}
	var sse strings.Builder
	for i, f := range fragments {
		role := ""
		if i == 0 {
			role = `"role":"assistant",`
		}
		fmt.Fprintf(&sse, "data: {\"choices\":[{\"index\":0,\"delta\":{%s\"reasoning_content\":%s}}]}\n\n", role, jsonString(f))
	}
	sse.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"答案是9只羊。\"},\"finish_reason\":\"stop\"}]}\n\n")
	sse.WriteString("data: [DONE]\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse.String())
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ShowReasoning: true, ReasoningCoalesce: true, ReasoningCoalesceInterval: 250 * time.Millisecond, ReasoningCoalesceChars: 1024}, "test", io.Discard)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"羊的问题"}],"stream":true}`)))
	got := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, got)
	}
	reasoningEvents := strings.Count(got, "reasoning_content")
	if reasoningEvents == 0 {
		t.Fatalf("reasoning must be present when enabled: %s", got)
	}
	if reasoningEvents > len(fragments)/2 {
		t.Fatalf("expected coalescing to reduce %d upstream deltas to fewer events, got %d: %s", len(fragments), reasoningEvents, got)
	}
	for _, f := range fragments {
		if !strings.Contains(got, f) {
			t.Fatalf("coalescing dropped fragment %q: %s", f, got)
		}
	}
	if !strings.Contains(got, "答案是9只羊。") {
		t.Fatalf("final content missing: %s", got)
	}
	if strings.Index(got, "reasoning_content") > strings.Index(got, "答案是9只羊。") {
		t.Fatalf("reasoning must precede final content: %s", got)
	}
}

// TestTextStreamingReasoningPassthroughWhenDisabled confirms the legacy 1:1
// behavior is preserved when REASONING_COALESCE is turned off.
func TestTextStreamingReasoningPassthroughWhenDisabled(t *testing.T) {
	fragments := []string{"a", "b", "c", "d", "e"}
	var sse strings.Builder
	for _, f := range fragments {
		fmt.Fprintf(&sse, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":%s}}]}\n\n", jsonString(f))
	}
	sse.WriteString("data: [DONE]\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse.String())
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ShowReasoning: true, ReasoningCoalesce: false}, "test", io.Discard)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	got := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, got)
	}
	if events := strings.Count(got, "reasoning_content"); events != len(fragments) {
		t.Fatalf("passthrough must keep %d events, got %d: %s", len(fragments), events, got)
	}
}

func TestReasoningParamsAutoInjected(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ShowReasoning: true}, "test", io.Discard)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if gotBody["reasoning_effort"] != "high" {
		t.Fatalf("expected reasoning_effort=high, got %v", gotBody["reasoning_effort"])
	}
	reasoning, ok := gotBody["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("unexpected reasoning payload: %v", gotBody["reasoning"])
	}
}

func TestReasoningParamsNotOverridden(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ShowReasoning: true}, "test", io.Discard)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"stream":false,"reasoning_effort":"low"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if gotBody["reasoning_effort"] != "low" {
		t.Fatalf("expected client reasoning_effort preserved, got %v", gotBody["reasoning_effort"])
	}
	if gotBody["reasoning"] != nil {
		t.Fatalf("expected no reasoning object when client only sent reasoning_effort, got %v", gotBody["reasoning"])
	}
}

func TestReasoningParamsNotInjectedWhenHidden(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ShowReasoning: false}, "test", io.Discard)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"stream":false}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if gotBody["reasoning_effort"] != nil || gotBody["reasoning"] != nil {
		t.Fatalf("reasoning params must not be injected when hidden: %v", gotBody)
	}
}

// TestReasoningParamsNestedReasoningGetsTopLevelEffort verifies Codex's fix #5:
// when the client supplies only the nested reasoning object, the proxy backfills
// the top-level reasoning_effort field that CodeBuddy needs to enable thinking,
// instead of returning early and leaving reasoning off.
func TestReasoningParamsNestedReasoningGetsTopLevelEffort(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: upstream.URL, DefaultModel: "hy3", RequestTimeout: time.Second, SessionTTL: time.Minute, ShowReasoning: true}, "test", io.Discard)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"stream":false,"reasoning":{"effort":"medium"}}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if gotBody["reasoning_effort"] != "medium" {
		t.Fatalf("expected top-level reasoning_effort=medium derived from nested reasoning, got %v", gotBody["reasoning_effort"])
	}
	reasoning, ok := gotBody["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "medium" {
		t.Fatalf("nested reasoning must be preserved: %v", gotBody["reasoning"])
	}
}

// timedResponseWriter records each Write together with the time it happened, so
// timing-sensitive behavior (e.g. the coalesce timer firing during an upstream
// pause) can be asserted. The proxy is the sole writer, so no locking is needed.
type timedResponseWriter struct {
	code   int
	header http.Header
	chunks []string
	times  []time.Time
}

func (t *timedResponseWriter) Header() http.Header {
	if t.header == nil {
		t.header = http.Header{}
	}
	return t.header
}
func (t *timedResponseWriter) Write(b []byte) (int, error) {
	t.chunks = append(t.chunks, string(b))
	t.times = append(t.times, time.Now())
	return len(b), nil
}
func (t *timedResponseWriter) WriteHeader(c int) { t.code = c }
func (t *timedResponseWriter) Flush()            {}

func writeReasoningChunk(w io.Writer, index int, text string) {
	fmt.Fprintf(w, "data: {\"choices\":[{\"index\":%d,\"delta\":{\"reasoning_content\":%s}}]}\n\n", index, jsonString(text))
}
func writeContentChunk(w io.Writer, index int, text, finish string) {
	fmt.Fprintf(w, "data: {\"choices\":[{\"index\":%d,\"delta\":{\"content\":%s},\"finish_reason\":%s}]}\n\n", index, jsonString(text), jsonString(finish))
}
func indexOfChunkWith(w *timedResponseWriter, substr string) int {
	for i, c := range w.chunks {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

// TestTextStreamingReasoningCoalesceTimerFiresDuringPause proves Codex's P1:
// the coalescing window is driven by an active timer, not merely by the arrival
// of the next upstream chunk. After buffering reasoning, if the upstream goes
// silent for longer than REASONING_COALESCE_MS, the proxy must emit the buffered
// reasoning on its own — before the stream ends and before any later chunk.
func TestTextStreamingReasoningCoalesceTimerFiresDuringPause(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive test")
	}
	const interval = 150 * time.Millisecond
	pr, pw := io.Pipe()
	started := make(chan struct{})
	t0 := time.Now()
	go func() {
		defer pw.Close()
		<-started
		writeReasoningChunk(pw, 0, "first")  // emitted immediately (firstReasoningSent)
		writeReasoningChunk(pw, 0, "second") // buffered; window armed
		// Upstream falls silent. Without an active timer the proxy would hold
		// "second" until the next chunk / stream end.
		time.Sleep(400 * time.Millisecond)
		writeContentChunk(pw, 0, "answer", "stop")
	}()
	h := NewWithLogWriter(Config{APIKey: "ck_example_12345678", BaseURL: "http://example.invalid", DefaultModel: "hy3", RequestTimeout: 5 * time.Second, SessionTTL: time.Minute, ShowReasoning: true, ReasoningCoalesce: true, ReasoningCoalesceInterval: interval}, "test", io.Discard)
	w := &timedResponseWriter{}
	close(started)
	if _, err := h.relaySSE(w, pr, "hy3", reasoningCoalesce); err != nil {
		t.Fatalf("relaySSE err: %v", err)
	}
	firstIdx := indexOfChunkWith(w, `"first"`)
	secondIdx := indexOfChunkWith(w, `"second"`)
	answerIdx := indexOfChunkWith(w, "answer")
	if firstIdx < 0 || secondIdx < 0 || answerIdx < 0 {
		t.Fatalf("missing events: %v", w.chunks)
	}
	if secondIdx >= answerIdx {
		t.Fatalf("timer-flushed reasoning (second) must precede final content: %v", w.chunks)
	}
	// The "second" reasoning event must have been emitted during the upstream
	// pause (well before the content chunk written at ~t0+400ms), proving the
	// timer fired on its own rather than on the next chunk's arrival.
	if w.times[secondIdx].Sub(t0) >= 350*time.Millisecond {
		t.Fatalf("coalesce timer did not fire during pause; second event at %v (t0=%v): %v", w.times[secondIdx], t0, w.chunks)
	}
}
