//go:build live

package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func newLiveHandler(t *testing.T) *Handler {
	t.Helper()
	if os.Getenv("CODEBUDDY_LIVE_TEST") != "1" {
		t.Skip("set CODEBUDDY_LIVE_TEST=1 to enable live tests")
	}
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return NewWithLogWriter(cfg, "live-test", io.Discard)
}

func TestLiveModelConfiguration(t *testing.T) {
	h := newLiveHandler(t)
	candidates := []struct{ path, product, userAgent, productVersion string }{
		{"/config/models", "", "", ""}, {"/v2/config", "", "", ""}, {"/v3/config", "", "", ""}, {"/v3/config", "CLI", "", ""},
		{"/v3/config", "", "CLI/2.130.0", "2.130.0"}, {"/v3/config", "", "CodeBuddy/2.130.0", "2.130.0"}, {"/v3/config", "", "CodeBuddyCLI/2.130.0", "2.130.0"},
		{"/v3/config", "", "codebuddy-cli/2.130.0", "2.130.0"}, {"/v3/config", "", "CodingCopilot/2.130.0", "2.130.0"}, {"/v3/config", "", "coding-copilot/2.130.0", "2.130.0"},
		{"/v2/models", "", "", ""},
	}
	for _, candidate := range candidates {
		path := candidate.path
		req, err := http.NewRequest(http.MethodGet, "https://copilot.tencent.com"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range h.headers(newSession()) {
			req.Header.Set(k, v)
		}
		req.Header.Set("Connection", "close")
		if candidate.product != "" {
			req.Header.Set("X-Product", candidate.product)
		}
		if candidate.userAgent != "" {
			req.Header.Set("User-Agent", candidate.userAgent)
			req.Header.Set("X-Product-Version", candidate.productVersion)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		var value any
		_ = json.Unmarshal(raw, &value)
		t.Logf("%s product=%q user_agent=%q status=%d top_level_keys=%v data_keys=%v code=%v message=%q models=%v", path, candidate.product, candidate.userAgent, resp.StatusCode, topLevelKeys(value), topLevelKeys(mapValue(value, "data")), mapValue(value, "code"), firstNonEmpty(stringValue(mapValue(value, "msg")), stringValue(mapValue(value, "message")), stringValue(mapValue(value, "error_msg"))), modelIDs(value))
	}
}

func TestLiveProxyModels(t *testing.T) {
	h := newLiveHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("models endpoint returned status %d", rr.Code)
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) == 0 {
		t.Fatal("models endpoint returned an empty list")
	}
	t.Logf("models endpoint source=%s count=%d", rr.Header().Get("X-CodeBuddy-Models-Source"), len(response.Data))
}

func mapValue(value any, key string) any {
	object, _ := value.(map[string]any)
	return object[key]
}
func modelIDs(value any) []string {
	data, _ := mapValue(value, "data").(map[string]any)
	models, _ := data["models"].([]any)
	ids := make([]string, 0, len(models))
	for _, raw := range models {
		model, _ := raw.(map[string]any)
		if id := stringValue(model["id"]); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestLiveImageInput(t *testing.T) {
	h := newLiveHandler(t)
	imageData := solidPNGBase64(t, color.RGBA{R: 255, A: 255})
	prompt := "请观察图片的主色。只回答颜色名称。"
	expected := "红"
	if imagePath := os.Getenv("CODEBUDDY_LIVE_IMAGE"); imagePath != "" {
		data, err := os.ReadFile(imagePath)
		if err != nil {
			t.Fatal(err)
		}
		imageData = base64.StdEncoding.EncodeToString(data)
		prompt = "请阅读这张图片。图片第一行的网络名称是什么？只回答该名称。"
		expected = "bridge"
	}
	payload, _ := json.Marshal(map[string]any{
		"model":  "hy3",
		"stream": false,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": prompt},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + imageData}},
			},
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	started := time.Now()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("image request returned status %d after %s", rr.Code, time.Since(started).Round(time.Millisecond))
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("image request returned invalid JSON: %v", err)
	}
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("image request completed without choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content := stringValue(message["content"])
	if !strings.Contains(strings.ToLower(content), strings.ToLower(expected)) {
		t.Fatalf("image response did not contain expected answer %q", expected)
	}
	t.Logf("image understanding succeeded in %s: %q", time.Since(started).Round(time.Millisecond), content)
}

func TestLiveImageStreamingReasoning(t *testing.T) {
	h := newLiveHandler(t)
	imageData := solidPNGBase64(t, color.RGBA{R: 255, A: 255})
	payload, _ := json.Marshal(map[string]any{
		"model": "hy3", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "请观察图片的主色。只回答颜色名称。"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + imageData}},
		}}},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload)))
	if rr.Code != http.StatusOK {
		t.Fatalf("streaming image request returned status %d", rr.Code)
	}
	var reasoningChunks, contentChunks int
	for _, event := range strings.Split(rr.Body.String(), "\n\n") {
		data := strings.TrimSpace(strings.TrimPrefix(event, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, raw := range sliceValue(chunk["choices"]) {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if stringValue(delta["reasoning_content"]) != "" {
				reasoningChunks++
			}
			if stringValue(delta["content"]) != "" {
				contentChunks++
			}
		}
	}
	t.Logf("streaming image response: reasoning_chunks=%d content_chunks=%d", reasoningChunks, contentChunks)
}

func solidPNGBase64(t *testing.T, pixel color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			img.SetRGBA(x, y, pixel)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func topLevelKeys(value any) []string {
	object, _ := value.(map[string]any)
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
