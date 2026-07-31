// Package proxy implements the dependency-free CodeBuddy OpenAI-compatible gateway.
package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var knownModels = []string{"auto", "hy3", "glm-5.2", "glm-5.1", "glm-5v-turbo", "minimax-m3", "kimi-k3-1", "kimi-k2.7", "kimi-k2.6", "deepseek-v4-flash", "deepseek-v4-pro"}

const cliVersion = "2.130.0"
const maxBufferedReasoningBytes = 1 << 20

type Config struct {
	APIKey, BaseURL, Host, Port, DefaultModel   string
	RequestTimeout, SessionTTL, ModelRefreshTTL time.Duration
	ShowReasoning                               bool
	// ReasoningCoalesce applies bounded windowed coalescing to text requests so
	// the client receives far fewer reasoning_content events without waiting for
	// the whole reasoning phase to finish (low first-token latency).
	ReasoningCoalesce         bool
	ReasoningCoalesceInterval time.Duration
	ReasoningCoalesceChars    int
	// AggregateImageReasoning restores the legacy behavior where image requests
	// buffer the entire reasoning phase into a single event. It is off by default
	// because it delays reasoning until thinking finishes; image requests now use
	// the same progressive coalescing as text.
	AggregateImageReasoning bool
}

func LoadConfigFromEnv() (Config, error) {
	key := firstEnv("CODEBUDDY_API_KEY", "CODEBUDDY_TOKEN")
	if key == "" {
		return Config{}, errors.New("CODEBUDDY_API_KEY is required")
	}
	return Config{
		APIKey: key, BaseURL: resolveBaseURL(os.Getenv("CODEBUDDY_BASE_URL"), os.Getenv("CODEBUDDY_INTERNET_ENVIRONMENT")),
		Host: valueOr(os.Getenv("HOST"), "0.0.0.0"), Port: valueOr(os.Getenv("PORT"), "8787"), DefaultModel: valueOr(os.Getenv("DEFAULT_MODEL"), "hy3"),
		RequestTimeout: durationEnv("REQUEST_TIMEOUT_MS", 10*time.Minute), SessionTTL: durationEnv("SESSION_TTL_MS", 30*time.Minute), ModelRefreshTTL: durationEnv("MODEL_REFRESH_INTERVAL_MS", 10*time.Minute),
		ShowReasoning:             os.Getenv("HIDE_REASONING") != "1",
		ReasoningCoalesce:         os.Getenv("REASONING_COALESCE") != "0",
		ReasoningCoalesceInterval: durationEnv("REASONING_COALESCE_MS", 250*time.Millisecond),
		ReasoningCoalesceChars:    intEnv("REASONING_COALESCE_CHARS", 1024),
		AggregateImageReasoning:   os.Getenv("REASONING_AGGREGATE_IMAGES") == "1",
	}, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
func valueOr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
func durationEnv(name string, fallback time.Duration) time.Duration {
	if n, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil && n > 0 {
		return time.Duration(n) * time.Millisecond
	}
	return fallback
}
func intEnv(name string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return fallback
}
func resolveBaseURL(explicit, environment string) string {
	if explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	if strings.EqualFold(environment, "internal") || strings.EqualFold(environment, "ioa") {
		return "https://copilot.tencent.com/v2"
	}
	return "https://www.codebuddy.ai/v2"
}

type identity struct {
	Kind, UserID, Domain string
	ExpiresAt            time.Time
}

func tokenIdentity(token string) identity {
	p := strings.Split(token, ".")
	if len(p) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(p[1]); err == nil {
			var claims struct {
				Sub, Iss string
				Exp      int64
			}
			if json.Unmarshal(raw, &claims) == nil {
				i := identity{Kind: "jwt", UserID: claims.Sub}
				if u, err := url.Parse(claims.Iss); err == nil {
					i.Domain = u.Hostname()
				}
				if claims.Exp > 0 {
					i.ExpiresAt = time.Unix(claims.Exp, 0)
				}
				return i
			}
		}
	}
	tail := token
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return identity{Kind: "apiKey", UserID: "anonymous_" + tail}
}
func expired(token string) bool {
	i := tokenIdentity(token)
	return !i.ExpiresAt.IsZero() && time.Until(i.ExpiresAt) <= 30*time.Second
}

type session struct {
	ConversationID, ConversationRequestID, TraceID, RootSpanID string
	LastUsed                                                   time.Time
}
type sessionStore struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]*session
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, items: make(map[string]*session)}
}
func (s *sessionStore) acquire(key string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.items {
		if now.Sub(v.LastUsed) > s.ttl {
			delete(s.items, k)
		}
	}
	if key == "" {
		return newSession()
	}
	v := s.items[key]
	if v == nil {
		v = newSession()
		s.items[key] = v
	}
	v.LastUsed = now
	return v
}
func (s *sessionStore) size() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.items) }
func newSession() *session {
	v := &session{ConversationID: uuid(), LastUsed: time.Now()}
	v.beginTurn()
	return v
}
func (s *session) beginTurn() {
	s.ConversationRequestID, s.TraceID, s.RootSpanID, s.LastUsed = hexN(16), hexN(16), hexN(8), time.Now()
}
func (s *session) requestIDs() (string, string, string) { return hexN(16), hexN(8), s.RootSpanID }
func hexN(n int) string                                 { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type Handler struct {
	cfg      Config
	version  string
	client   *http.Client
	sessions *sessionStore
	logger   *textLogger
	catalog  *modelCatalog
}

func New(cfg Config, version string) *Handler {
	return NewWithLogWriter(cfg, version, os.Stdout)
}

// NewWithLogWriter creates a handler that writes Logback-style text logs to output.
func NewWithLogWriter(cfg Config, version string, output io.Writer) *Handler {
	if output == nil {
		output = os.Stdout
	}
	return &Handler{cfg: cfg, version: version, client: &http.Client{Timeout: cfg.RequestTimeout}, sessions: newSessionStore(cfg.SessionTTL), logger: newTextLogger(output), catalog: newModelCatalog(cfg.ModelRefreshTTL)}
}

// LogStartup records the safe, operational details needed to identify a running instance.
func (h *Handler) LogStartup(address string) {
	h.logger.info("server started", "version", h.version, "address", address, "upstream", h.cfg.BaseURL)
}

// LogServerError records a server-level error without including request data.
func (h *Handler) LogServerError(err error) {
	h.logger.error("server stopped", "error", err)
}

type textLogger struct{ output *log.Logger }

func newTextLogger(output io.Writer) *textLogger          { return &textLogger{output: log.New(output, "", 0)} }
func (l *textLogger) info(message string, fields ...any)  { l.write("INFO", message, fields...) }
func (l *textLogger) warn(message string, fields ...any)  { l.write("WARN", message, fields...) }
func (l *textLogger) error(message string, fields ...any) { l.write("ERROR", message, fields...) }
func (l *textLogger) write(level, message string, fields ...any) {
	var line strings.Builder
	fmt.Fprintf(&line, "%s %-5s [codebuddy-proxy] %s", time.Now().Format("2006-01-02 15:04:05.000"), level, message)
	for i := 0; i+1 < len(fields); i += 2 {
		fmt.Fprintf(&line, " %s=%s", fields[i], logField(fields[i+1]))
	}
	l.output.Print(line.String())
}
func logField(value any) string {
	switch v := value.(type) {
	case string:
		if strings.ContainsAny(v, " \t\r\n=\"") {
			return strconv.Quote(v)
		}
		return v
	case error:
		return strconv.Quote(v.Error())
	default:
		return fmt.Sprint(v)
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Session-Id")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "/health" || path == "/v1/health" {
		h.health(w)
		return
	}
	if r.Method == http.MethodGet && (path == "/v1/models" || path == "/models") {
		h.models(w, r)
		return
	}
	if r.Method == http.MethodPost && (path == "/v1/chat/completions" || path == "/chat/completions") {
		h.chat(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "unknown route: "+path, "not_found", nil)
}

func (h *Handler) health(w http.ResponseWriter) {
	i := tokenIdentity(h.cfg.APIKey)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "mode": "direct", "version": h.version, "upstream": h.cfg.BaseURL, "sessions": h.sessions.size(), "auth": map[string]any{"kind": i.Kind, "userId": i.UserID, "domain": i.Domain, "expired": expired(h.cfg.APIKey)}})
}
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"
	models, dynamic, err := h.catalog.list(r.Context(), force, h.fetchModels)
	if err != nil {
		h.logger.warn("model catalog refresh failed; using cached models", "error", err)
	}
	out := make([]map[string]any, 0, len(models))
	now := time.Now().Unix()
	for _, m := range models {
		out = append(out, map[string]any{"id": m, "object": "model", "created": now, "owned_by": "codebuddy"})
	}
	if dynamic {
		w.Header().Set("X-CodeBuddy-Models-Source", "remote")
	} else {
		w.Header().Set("X-CodeBuddy-Models-Source", "fallback")
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

type modelCatalog struct {
	mu      sync.Mutex
	ttl     time.Duration
	models  []string
	checked time.Time
	dynamic bool
}

func newModelCatalog(ttl time.Duration) *modelCatalog {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &modelCatalog{ttl: ttl, models: append([]string(nil), knownModels...)}
}
func (c *modelCatalog) list(ctx context.Context, force bool, fetch func(context.Context) ([]string, error)) ([]string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && !c.checked.IsZero() && time.Since(c.checked) < c.ttl {
		return append([]string(nil), c.models...), c.dynamic, nil
	}
	models, err := fetch(ctx)
	c.checked = time.Now()
	if err != nil {
		return append([]string(nil), c.models...), c.dynamic, err
	}
	c.models, c.dynamic = models, true
	return append([]string(nil), c.models...), c.dynamic, nil
}

func (h *Handler) fetchModels(ctx context.Context) ([]string, error) {
	endpoint, err := modelConfigURL(h.cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range h.headers(newSession()) {
		req.Header.Set(k, v)
	}
	// The configuration service validates this official CLI user-agent format.
	req.Header.Set("User-Agent", "CodeBuddy/"+cliVersion)
	req.Header.Set("X-Product-Version", cliVersion)
	req.Header.Set("Connection", "close")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("model configuration returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Models []struct {
				ID       string `json:"id"`
				Disabled bool   `json:"disabled"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return nil, err
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("model configuration failed: %s", result.Msg)
	}
	ids := make([]string, 0, len(result.Data.Models))
	seen := make(map[string]struct{}, len(result.Data.Models))
	for _, model := range result.Data.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" || model.Disabled {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("model configuration returned no enabled models")
	}
	sort.Strings(ids)
	return ids, nil
}
func modelConfigURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid CodeBuddy base URL")
	}
	u.Path, u.RawQuery, u.Fragment = "/v3/config", "", ""
	return u.String(), nil
}

func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error", nil)
		return
	}
	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) == 0 {
		writeError(w, http.StatusBadRequest, "messages must be a non-empty array", "invalid_request_error", nil)
		return
	}
	if expired(h.cfg.APIKey) {
		writeError(w, http.StatusUnauthorized, "CODEBUDDY_API_KEY has expired; refresh it and restart", "authentication_error", nil)
		return
	}
	wantStream, _ := payload["stream"].(bool)
	hasImage := hasImageInput(sliceValue(payload["messages"]))
	model := h.cfg.DefaultModel
	if v, ok := payload["model"].(string); ok && strings.TrimSpace(v) != "" {
		model = strings.TrimSpace(v)
	}
	payload["model"] = model
	payload["stream"] = true
	if h.cfg.ShowReasoning {
		ensureReasoningParams(payload)
	}
	key := firstNonEmpty(r.Header.Get("X-Session-Id"), r.Header.Get("X-Conversation-Id"), stringValue(payload["session_id"]), stringValue(payload["user"]))
	s := h.sessions.acquire(key)
	s.beginTurn()
	started := time.Now()
	prompt := formatPrompts(msgs)
	h.logger.info("chat request started", "model", model, "stream", wantStream, "message_count", len(msgs), "session_reused", key != "", "prompt", prompt)
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(h.cfg.BaseURL, "/")+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		h.logger.error("chat request failed", "model", model, "stream", wantStream, "stage", "create_upstream_request", "duration_ms", time.Since(started).Milliseconds())
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error", nil)
		return
	}
	for k, v := range h.headers(s) {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.error("chat request failed", "model", model, "stream", wantStream, "stage", "connect_upstream", "duration_ms", time.Since(started).Milliseconds())
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", nil)
		return
	}
	defer resp.Body.Close()
	upstreamMS := time.Since(started).Milliseconds()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.logger.warn("upstream response failed", "model", model, "stream", wantStream, "upstream_status", resp.StatusCode, "upstream_ms", upstreamMS, "duration_ms", time.Since(started).Milliseconds())
		h.upstreamFailure(w, resp)
		return
	}
	if wantStream {
		stats, err := h.relaySSE(w, resp.Body, model, h.reasoningModeFor(hasImage))
		attrs := []any{"model", model, "stream", true, "chunk_count", stats.chunks, "upstream_ms", upstreamMS, "duration_ms", time.Since(started).Milliseconds()}
		if !stats.firstToken.IsZero() {
			attrs = append(attrs, "ttft_ms", stats.firstToken.Sub(started).Milliseconds())
		}
		attrs = append(attrs, stats.usage.logFields()...)
		if err != nil {
			attrs = append(attrs, "stage", "read_upstream_stream")
			h.logger.error("chat request failed", attrs...)
			return
		}
		h.logger.info("chat request completed", attrs...)
		return
	}
	chunks, err := readSSE(resp.Body, model)
	if err != nil {
		h.logger.error("chat request failed", "model", model, "stream", false, "stage", "read_upstream_stream", "upstream_ms", upstreamMS, "duration_ms", time.Since(started).Milliseconds())
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", nil)
		return
	}
	if !h.cfg.ShowReasoning {
		removeReasoning(chunks)
	}
	writeJSON(w, http.StatusOK, aggregate(chunks, model))
	h.logger.info("chat request completed", append([]any{"model", model, "stream", false, "chunk_count", len(chunks), "upstream_ms", upstreamMS, "duration_ms", time.Since(started).Milliseconds()}, usageForChunks(chunks).logFields()...)...)
}

func hasImageInput(messages []any) bool {
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		for _, rawPart := range sliceValue(message["content"]) {
			part, _ := rawPart.(map[string]any)
			if part["type"] == "image_url" {
				return true
			}
		}
	}
	return false
}

func (h *Handler) headers(s *session) map[string]string {
	messageID, spanID, parentID := s.requestIDs()
	i := tokenIdentity(h.cfg.APIKey)
	headers := map[string]string{"Accept": "application/json", "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest", "X-Stainless-Arch": runtimeArch(), "X-Stainless-Lang": "js", "X-Stainless-OS": runtimeOS(), "X-Stainless-Package-Version": "6.25.0", "X-Stainless-Retry-Count": "0", "X-Stainless-Timeout": "600", "X-Stainless-Runtime": "node", "X-Stainless-Runtime-Version": "v24", "X-Conversation-ID": s.ConversationID, "X-Conversation-Request-ID": s.ConversationRequestID, "X-Conversation-Message-ID": messageID, "X-Request-ID": messageID, "X-Agent-Intent": "craft", "X-Agent-Purpose": "conversation", "X-IDE-Type": "CLI", "X-IDE-Name": "CLI", "X-IDE-Version": "2.130.0", "X-Private-Data": "false", "X-Product": "SaaS", "User-Agent": "axios/1.18.1"}
	headers["Authorization"], headers["X-API-Key"], headers["X-User-Id"] = "Bearer "+h.cfg.APIKey, h.cfg.APIKey, i.UserID
	if i.Domain != "" {
		headers["X-Domain"] = i.Domain
	}
	headers["Traceparent"] = "00-" + s.TraceID + "-" + spanID + "-01"
	headers["B3"] = s.TraceID + "-" + spanID + "-1-" + parentID
	headers["X-B3-TraceId"], headers["X-B3-ParentSpanId"], headers["X-B3-SpanId"], headers["X-B3-Sampled"], headers["X-Trace-ID"] = s.TraceID, parentID, spanID, "1", s.TraceID
	return headers
}
func runtimeArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}
func runtimeOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return "Unknown"
	}
}
func stringValue(v any) string { s, _ := v.(string); return s }
func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func formatPrompts(messages []any) string {
	parts := make([]string, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := valueOr(stringValue(message["role"]), "unknown")
		content := promptContent(message["content"])
		parts = append(parts, "["+role+"] "+content)
	}
	return strings.Join(parts, "\n")
}
func promptContent(value any) string {
	if text := stringValue(value); text != "" {
		return text
	}
	parts, ok := value.([]any)
	if !ok {
		return fmt.Sprint(value)
	}
	text := make([]string, 0, len(parts))
	for _, raw := range parts {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if v := firstNonEmpty(stringValue(item["text"]), stringValue(item["content"])); v != "" {
			text = append(text, v)
		}
	}
	return strings.Join(text, "\n")
}

type usageMetrics struct {
	prompt, completion, total, cached int64
}

func (u usageMetrics) logFields() []any {
	return []any{"prompt_tokens", u.prompt, "completion_tokens", u.completion, "total_tokens", u.total, "cached_tokens", u.cached}
}
func usageForChunks(chunks []map[string]any) usageMetrics {
	var usage usageMetrics
	for _, chunk := range chunks {
		if raw, ok := chunk["usage"].(map[string]any); ok {
			usage = usageFromMap(raw)
		}
	}
	return usage
}
func usageFromMap(raw map[string]any) usageMetrics {
	u := usageMetrics{
		prompt: tokenCount(raw, "prompt_tokens", "input_tokens"), completion: tokenCount(raw, "completion_tokens", "output_tokens"), total: tokenCount(raw, "total_tokens"),
		cached: tokenCount(raw, "cached_tokens", "cache_read_input_tokens"),
	}
	if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
		if cached := tokenCount(details, "cached_tokens", "cache_read_input_tokens"); cached > 0 {
			u.cached = cached
		}
	}
	return u
}
func tokenCount(raw map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch v := raw[key].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		case json.Number:
			n, _ := v.Int64()
			return n
		}
	}
	return 0
}

func (h *Handler) upstreamFailure(w http.ResponseWriter, resp *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	msg := stringValue(data["msg"])
	if msg == "" {
		if e, ok := data["error"].(map[string]any); ok {
			msg = stringValue(e["message"])
		}
	}
	if msg == "" {
		msg = string(raw)
	}
	status := http.StatusBadGateway
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		status = resp.StatusCode
	}
	writeError(w, status, msg, "upstream_error", data["code"])
}

type streamStats struct {
	chunks     int
	firstToken time.Time
	usage      usageMetrics
}

// reasoningMode controls how upstream reasoning_content deltas are forwarded.
type reasoningMode int

const (
	reasoningOff         reasoningMode = iota // strip all reasoning from the response
	reasoningAggregate                        // buffer the whole reasoning phase into a single event (images)
	reasoningCoalesce                         // bounded windowed coalescing for text (low latency, few events)
	reasoningPassthrough                      // forward reasoning deltas 1:1 (legacy text behavior)
)

// reasoningModeFor selects the reasoning handling strategy for a request.
func (h *Handler) reasoningModeFor(hasImage bool) reasoningMode {
	if !h.cfg.ShowReasoning {
		return reasoningOff
	}
	if hasImage && h.cfg.AggregateImageReasoning {
		// Legacy opt-in: one aggregated reasoning event per image request. This
		// necessarily withholds reasoning until the whole thinking phase ends.
		return reasoningAggregate
	}
	// Images use the same progressive coalescing as text so reasoning streams as
	// it is produced instead of arriving in one late burst.
	if h.cfg.ReasoningCoalesce {
		return reasoningCoalesce
	}
	return reasoningPassthrough
}

func (h *Handler) relaySSE(w http.ResponseWriter, body io.Reader, model string, mode reasoningMode) (streamStats, error) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var stats streamStats
	bufferedReasoning := make(map[int]*boundedText)
	firstReasoningSent := false

	emit := func(chunk map[string]any) {
		if stats.firstToken.IsZero() {
			stats.firstToken = time.Now()
		}
		stats.chunks++
		if rawUsage, ok := chunk["usage"].(map[string]any); ok {
			stats.usage = usageFromMap(rawUsage)
		}
		normalized, _ := json.Marshal(normalize(chunk, model))
		fmt.Fprintf(w, "data: %s\n\n", normalized)
		if flusher != nil {
			flusher.Flush()
		}
	}
	flushRaw := func(format string, args ...any) {
		fmt.Fprintf(w, format, args...)
		if flusher != nil {
			flusher.Flush()
		}
	}
	done := func() { flushRaw("data: [DONE]\n\n") }

	// Flush timer drives the coalescing window so buffered reasoning is emitted on
	// its own cadence, instead of only when the next upstream chunk happens to
	// arrive. It stays stopped unless a reasoning window is open, and the main
	// loop is the only writer of w, so the timer never races with emit.
	flushTimer := time.NewTimer(time.Hour)
	flushTimer.Stop()
	stopFlushTimer := func() {
		if !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
	}
	armFlushTimer := func() {
		stopFlushTimer()
		flushTimer.Reset(h.cfg.ReasoningCoalesceInterval)
	}
	flushReasoning := func() {
		stopFlushTimer()
		if len(bufferedReasoning) == 0 {
			return
		}
		indexes := make([]int, 0, len(bufferedReasoning))
		for index := range bufferedReasoning {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		choices := make([]any, 0, len(indexes))
		for _, index := range indexes {
			if text := bufferedReasoning[index].String(); text != "" {
				// Include role:"assistant" so strict clients see a well-formed
				// delta, matching the initial role event from the upstream.
				choices = append(choices, map[string]any{"index": index, "delta": map[string]any{"role": "assistant", "reasoning_content": text}, "finish_reason": nil})
			}
		}
		bufferedReasoning = make(map[int]*boundedText)
		if len(choices) > 0 {
			emit(map[string]any{"choices": choices})
		}
	}

	// Read upstream SSE lines in a dedicated goroutine and feed them through a
	// channel, so the main loop can also react to the flush timer without
	// depending on the next upstream chunk. The reader only writes to the channel,
	// keeping the ResponseWriter single-writer.
	lines := make(chan string, 64)
	var scanErr error
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64<<10), 8<<20)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanErr = scanner.Err()
	}()

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				flushReasoning()
				done()
				return stats, scanErr
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				flushReasoning()
				done()
				return stats, nil
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) != nil {
				flushReasoning()
				flushRaw("data: %s\n\n", data)
				continue
			}
			switch mode {
			case reasoningOff:
				removeReasoning([]map[string]any{chunk})
				if !hasVisibleSSEData(chunk) {
					continue
				}
			case reasoningAggregate:
				bufferReasoning(chunk, bufferedReasoning)
				if requiresReasoningFlush(chunk) {
					flushReasoning()
				}
				if !hasVisibleSSEData(chunk) {
					continue
				}
			case reasoningCoalesce:
				bufferReasoning(chunk, bufferedReasoning)
				if !firstReasoningSent {
					// First reasoning characters go out immediately (zero
					// perceived latency); later deltas are coalesced by window.
					flushReasoning()
					firstReasoningSent = true
				} else if len(bufferedReasoning) > 0 {
					armFlushTimer()
				}
				if requiresReasoningFlush(chunk) {
					flushReasoning()
				}
				if !hasVisibleSSEData(chunk) {
					continue
				}
			case reasoningPassthrough:
				// forward reasoning deltas verbatim, do not strip
			}
			emit(chunk)
		case <-flushTimer.C:
			if mode == reasoningCoalesce && len(bufferedReasoning) > 0 {
				flushReasoning()
			}
		}
	}
}

type boundedText struct {
	text      strings.Builder
	truncated bool
}

func (b *boundedText) Add(value string) {
	if value == "" || b.truncated {
		return
	}
	remaining := maxBufferedReasoningBytes - b.text.Len()
	if remaining <= 0 {
		b.truncated = true
		return
	}
	if len(value) > remaining {
		b.text.WriteString(value[:remaining])
		b.truncated = true
		return
	}
	b.text.WriteString(value)
}
func (b *boundedText) String() string {
	if b.truncated {
		return b.text.String() + "\n\n[思考内容过长，已截断]"
	}
	return b.text.String()
}
func (b *boundedText) Len() int { return b.text.Len() }

func bufferReasoning(chunk map[string]any, buffered map[int]*boundedText) {
	for _, raw := range sliceValue(chunk["choices"]) {
		choice, _ := raw.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		text := stringValue(delta["reasoning_content"])
		delete(delta, "reasoning_content")
		if text == "" {
			continue
		}
		index := int(numberValue(choice["index"]))
		if buffered[index] == nil {
			buffered[index] = &boundedText{}
		}
		buffered[index].Add(text)
	}
}

func requiresReasoningFlush(chunk map[string]any) bool {
	for _, raw := range sliceValue(chunk["choices"]) {
		choice, _ := raw.(map[string]any)
		if finish := choice["finish_reason"]; finish != nil && finish != "" {
			return true
		}
		delta, _ := choice["delta"].(map[string]any)
		if stringValue(delta["content"]) != "" {
			return true
		}
		// An empty tool_calls array ([]) is just the default shape of every delta
		// and must NOT trigger a flush; only a non-empty array means a real tool
		// call. The upstream sends "tool_calls":[] on every delta.
		if toolCalls, ok := delta["tool_calls"].([]any); ok && len(toolCalls) > 0 {
			return true
		}
	}
	return false
}
func readSSE(body io.Reader, model string) ([]map[string]any, error) {
	var out []map[string]any
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(data), &c); err == nil {
			out = append(out, normalize(c, model))
		}
	}
	return out, scanner.Err()
}
func removeReasoning(chunks []map[string]any) {
	for _, chunk := range chunks {
		for _, raw := range sliceValue(chunk["choices"]) {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			delete(delta, "reasoning_content")
		}
	}
}

// ensureReasoningParams injects the reasoning parameters CodeBuddy needs to
// enable thinking, while respecting an explicit client choice. Default behavior
// (neither field supplied) auto-enables reasoning at "high" effort. If the
// client supplied only one form, the missing complementary field is derived so
// the upstream reliably turns thinking on — CodeBuddy requires the top-level
// reasoning_effort field, so a nested-only request gets it backfilled.
func ensureReasoningParams(payload map[string]any) {
	_, hasEff := payload["reasoning_effort"]
	reasoning, hasRsn := payload["reasoning"]
	if hasEff && hasRsn {
		return
	}
	if !hasEff && !hasRsn {
		payload["reasoning_effort"] = "high"
		payload["reasoning"] = map[string]any{"effort": "high", "summary": "auto"}
		return
	}
	if hasRsn && !hasEff {
		effort := "high"
		if rmap, ok := reasoning.(map[string]any); ok {
			if e, ok := rmap["effort"].(string); ok && e != "" {
				effort = e
			}
		}
		payload["reasoning_effort"] = effort
	}
}
func hasVisibleSSEData(chunk map[string]any) bool {
	if usage, ok := chunk["usage"]; ok && usage != nil {
		return true
	}
	for _, raw := range sliceValue(chunk["choices"]) {
		choice, _ := raw.(map[string]any)
		if finish := choice["finish_reason"]; finish != nil && finish != "" {
			return true
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		// Preserve the initial role event (good client compatibility) and any
		// meaningful content field. Pure-noise deltas from the upstream
		// (e.g. content:"" alongside tool_calls:[]) are skipped. Judgment is
		// field-aware: a literal string "assistant" in content is real text and
		// must NOT be treated as empty.
		if _, ok := delta["role"]; ok {
			return true
		}
		if stringValue(delta["content"]) != "" {
			return true
		}
		if stringValue(delta["reasoning_content"]) != "" {
			return true
		}
		if toolCalls, ok := delta["tool_calls"].([]any); ok && len(toolCalls) > 0 {
			return true
		}
		if fc, ok := delta["function_call"].(map[string]any); ok && len(fc) > 0 {
			return true
		}
	}
	return false
}
func normalize(c map[string]any, model string) map[string]any {
	if _, ok := c["model"]; !ok {
		c["model"] = model
	}
	if _, ok := c["object"]; !ok {
		c["object"] = "chat.completion.chunk"
	}
	return c
}
func aggregate(chunks []map[string]any, model string) map[string]any {
	out := map[string]any{"id": fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()), "object": "chat.completion", "created": time.Now().Unix(), "model": model}
	choices := map[int]map[string]any{}
	for _, c := range chunks {
		for _, k := range []string{"id", "model", "created", "usage", "system_fingerprint"} {
			if v, ok := c[k]; ok {
				out[k] = v
			}
		}
		for _, raw := range sliceValue(c["choices"]) {
			choice, _ := raw.(map[string]any)
			idx := int(numberValue(choice["index"]))
			acc := choices[idx]
			if acc == nil {
				acc = map[string]any{"index": idx, "message": map[string]any{"role": "assistant", "content": ""}, "logprobs": nil, "finish_reason": nil}
				choices[idx] = acc
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				msg := acc["message"].(map[string]any)
				if v, ok := delta["role"].(string); ok {
					msg["role"] = v
				}
				if v, ok := delta["content"].(string); ok {
					msg["content"] = msg["content"].(string) + v
				}
				if v, ok := delta["reasoning_content"].(string); ok && v != "" {
					msg["reasoning_content"] = stringValue(msg["reasoning_content"]) + v
				}
				if v, ok := delta["tool_calls"]; ok {
					msg["tool_calls"] = v
				}
			}
			if v, ok := choice["finish_reason"]; ok && v != nil && v != "" {
				acc["finish_reason"] = v
			}
		}
	}
	ordered := make([]any, 0, len(choices))
	for i := 0; i < len(choices); i++ {
		if c := choices[i]; c != nil {
			ordered = append(ordered, c)
		}
	}
	if len(ordered) == 0 {
		ordered = []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": ""}, "finish_reason": "stop"}}
	}
	out["choices"] = ordered
	return out
}
func sliceValue(v any) []any    { a, _ := v.([]any); return a }
func numberValue(v any) float64 { n, _ := v.(float64); return n }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, message, typ string, code any) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": typ, "code": code}})
}
