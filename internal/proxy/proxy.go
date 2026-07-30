// Package proxy implements the dependency-free CodeBuddy OpenAI-compatible gateway.
package proxy

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var knownModels = []string{"auto", "hy3", "glm-5.2", "glm-5.1", "glm-5v-turbo", "minimax-m3", "kimi-k3-1", "kimi-k2.7", "kimi-k2.6", "deepseek-v4-flash", "deepseek-v4-pro"}

type Config struct {
	APIKey, ProxyAPIKey, BaseURL, Host, Port, DefaultModel string
	RequestTimeout, SessionTTL                             time.Duration
}

func LoadConfigFromEnv() (Config, error) {
	key := firstEnv("CODEBUDDY_API_KEY", "CODEBUDDY_TOKEN")
	if key == "" {
		return Config{}, errors.New("CODEBUDDY_API_KEY is required")
	}
	return Config{
		APIKey: key, ProxyAPIKey: os.Getenv("PROXY_API_KEY"), BaseURL: resolveBaseURL(os.Getenv("CODEBUDDY_BASE_URL"), os.Getenv("CODEBUDDY_INTERNET_ENVIRONMENT")),
		Host: valueOr(os.Getenv("HOST"), "0.0.0.0"), Port: valueOr(os.Getenv("PORT"), "8787"), DefaultModel: valueOr(os.Getenv("DEFAULT_MODEL"), "hy3"),
		RequestTimeout: durationEnv("REQUEST_TIMEOUT_MS", 10*time.Minute), SessionTTL: durationEnv("SESSION_TTL_MS", 30*time.Minute),
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
}

func New(cfg Config, version string) *Handler {
	return &Handler{cfg: cfg, version: version, client: &http.Client{Timeout: cfg.RequestTimeout}, sessions: newSessionStore(cfg.SessionTTL)}
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
	if h.cfg.ProxyAPIKey != "" && r.Header.Get("Authorization") != "Bearer "+h.cfg.ProxyAPIKey {
		writeError(w, http.StatusUnauthorized, "invalid proxy api key", "authentication_error", nil)
		return
	}
	if r.Method == http.MethodGet && (path == "/v1/models" || path == "/models") {
		h.models(w)
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
func (h *Handler) models(w http.ResponseWriter) {
	out := make([]map[string]any, 0, len(knownModels))
	now := time.Now().Unix()
	for _, m := range knownModels {
		out = append(out, map[string]any{"id": m, "object": "model", "created": now, "owned_by": "codebuddy"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error", nil)
		return
	}
	if msgs, ok := payload["messages"].([]any); !ok || len(msgs) == 0 {
		writeError(w, http.StatusBadRequest, "messages must be a non-empty array", "invalid_request_error", nil)
		return
	}
	if expired(h.cfg.APIKey) {
		writeError(w, http.StatusUnauthorized, "CODEBUDDY_API_KEY has expired; refresh it and restart", "authentication_error", nil)
		return
	}
	wantStream, _ := payload["stream"].(bool)
	model := h.cfg.DefaultModel
	if v, ok := payload["model"].(string); ok && modelKnown(v) {
		model = v
	}
	payload["model"] = model
	payload["stream"] = true
	key := firstNonEmpty(r.Header.Get("X-Session-Id"), r.Header.Get("X-Conversation-Id"), stringValue(payload["session_id"]), stringValue(payload["user"]))
	s := h.sessions.acquire(key)
	s.beginTurn()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(h.cfg.BaseURL, "/")+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error", nil)
		return
	}
	for k, v := range h.headers(s) {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", nil)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.upstreamFailure(w, resp)
		return
	}
	if wantStream {
		h.relaySSE(w, resp.Body, model)
		return
	}
	chunks, err := readSSE(resp.Body, model)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", nil)
		return
	}
	writeJSON(w, http.StatusOK, aggregate(chunks, model))
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
func modelKnown(v string) bool {
	for _, m := range knownModels {
		if v == m {
			return true
		}
	}
	return false
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
func (h *Handler) relaySSE(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				fmt.Fprint(w, "data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) == nil {
				normalized, _ := json.Marshal(normalize(chunk, model))
				fmt.Fprintf(w, "data: %s\n\n", normalized)
			} else {
				fmt.Fprintf(w, "data: %s\n\n", data)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
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
