package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codebuddy2api/internal/cred"
	"codebuddy2api/internal/pool"
	"codebuddy2api/internal/upstream"
)

// fakeUpstream 起一个假的 CodeBuddy 上游：
//   - POST /v2/chat/completions → 按 mode 返回 SSE / 401 / 429
//   - GET  /v2/models           → 返回 OpenAI 风格模型列表
func fakeUpstream(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v2/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","owned_by":"zhipu"},{"id":"auto"}]}`))
			return
		}
		if r.URL.Path != "/v2/chat/completions" {
			http.NotFound(w, r)
			return
		}
		// 校验认证头
		if r.Header.Get("Authorization") == "" && r.Header.Get("X-Api-Key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid_format"}`))
			return
		}
		// 上游只接受流式
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("upstream got invalid json: %v", err)
		}
		if got["stream"] != true {
			t.Errorf("upstream expected stream=true, got %v", got["stream"])
		}

		switch mode {
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid_format"}`))
		case "429":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			chunks := []string{
				`{"id":"c1","created":1700000000,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
				`{"id":"c1","created":1700000000,"model":"glm-5.2","choices":[{"index":0,"delta":{"content":"好"}}]}`,
				`{"id":"c1","created":1700000000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			}
			for _, c := range chunks {
				_, _ = io.WriteString(w, "data: "+c+"\n\n")
				fl.Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestHandler(t *testing.T, mode string, apiKey string) (*Handler, *pool.Pool) {
	t.Helper()
	up := upstream.NewWithBase(fakeUpstream(t, mode).URL)
	p := pool.New("") // 不落盘
	p.Add(&cred.Cred{UID: "acct-1", Nickname: "acct-1", Token: "ck_test", Kind: cred.KindAPIKey})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       apiKey,
		MaxRotate:    3,
		SoftCooldown: 50 * time.Millisecond,
		HardCooldown: 50 * time.Millisecond,
		ErrCooldown:  50 * time.Millisecond,
		ErrThreshold: 5,
	})
	return h, p
}

func do(t *testing.T, h *Handler, method, path, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthRequired(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "sk-1")
	if w := do(t, h, "GET", "/v1/models", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", w.Code)
	}
	if w := do(t, h, "GET", "/v1/models", "wrong-key", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: got %d, want 401", w.Code)
	}
	if w := do(t, h, "GET", "/v1/models", "sk-1", ""); w.Code != http.StatusOK {
		t.Fatalf("good key: got %d, want 200", w.Code)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	w := do(t, h, "GET", "/v1/models", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Object != "list" || len(out.Data) != 2 {
		t.Fatalf("unexpected payload: %s", w.Body.String())
	}
	if out.Data[0].ID != "auto" || out.Data[1].ID != "glm-5.2" {
		t.Fatalf("models not sorted: %+v", out.Data)
	}
}

func TestNonStreamAggregated(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	w := do(t, h, "POST", "/v1/chat/completions", "",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("object = %v", out["object"])
	}
	choices, _ := out["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("content = %v", msg["content"])
	}
	if out["usage"] == nil {
		t.Fatal("usage missing")
	}
}

func TestStreamPassthrough(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	w := do(t, h, "POST", "/v1/chat/completions", "",
		`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	var content strings.Builder
	sawDone := false
	sc := bufio.NewScanner(w.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if chunk["model"] != "glm-5.2" {
			t.Fatalf("chunk model = %v", chunk["model"])
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			d, _ := c.(map[string]any)["delta"].(map[string]any)
			if s, ok := d["content"].(string); ok {
				content.WriteString(s)
			}
		}
	}
	if !sawDone {
		t.Fatal("missing [DONE]")
	}
	if content.String() != "你好" {
		t.Fatalf("streamed content = %q", content.String())
	}
}

func TestUpstream401DisablesAccount(t *testing.T) {
	h, p := newTestHandler(t, "401", "")
	w := do(t, h, "POST", "/v1/chat/completions", "",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	st, ok := p.Status("acct-1")
	if !ok {
		t.Fatal("account missing")
	}
	if !st.Disabled {
		t.Fatalf("account should be disabled, got %+v", st)
	}
}

func TestUpstream429CooldownsAccount(t *testing.T) {
	h, p := newTestHandler(t, "429", "")
	w := do(t, h, "POST", "/v1/chat/completions", "",
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	st, _ := p.Status("acct-1")
	if !st.Cooling {
		t.Fatalf("account should be cooling, got %+v", st)
	}
	if st.Disabled {
		t.Fatal("rate limit must not disable the account")
	}
}

func TestHealthzAndAdmin(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	if w := do(t, h, "GET", "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatalf("healthz = %d", w.Code)
	} else if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz body = %s", w.Body.String())
	}
	if w := do(t, h, "GET", "/admin", "", ""); w.Code != http.StatusOK {
		t.Fatalf("admin = %d", w.Code)
	} else if !strings.Contains(w.Body.String(), "CodeBuddy2API") {
		t.Fatal("admin page missing marker")
	}
}

func TestValidationErrors(t *testing.T) {
	h, _ := newTestHandler(t, "ok", "")
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}]}`, // 缺 model
		`{"model":"glm-5.2","messages":[]}`,             // 缺 messages
		`not-json`,
	} {
		if w := do(t, h, "POST", "/v1/chat/completions", "", body); w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: got %d, want 400", body, w.Code)
		}
	}
}
