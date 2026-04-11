package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type qwenRefreshRecorder struct {
	calls int
}

func (r *qwenRefreshRecorder) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	r.calls++
	cloned := auth.Clone()
	if cloned.Metadata == nil {
		cloned.Metadata = map[string]any{}
	}
	cloned.Metadata["access_token"] = "refreshed-token"
	cloned.Metadata["refresh_token"] = "refresh-token"
	return cloned, nil
}

func clearQwenRateLimiter() {
	qwenRateLimiter.Lock()
	qwenRateLimiter.requests = make(map[string][]time.Time)
	qwenRateLimiter.Unlock()
}

func TestQwenExecutorParseSuffix(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		wantBase string
	}{
		{"no suffix", "qwen-max", "qwen-max"},
		{"with level suffix", "qwen-max(high)", "qwen-max"},
		{"with budget suffix", "qwen-max(16384)", "qwen-max"},
		{"complex model name", "qwen-plus-latest(medium)", "qwen-plus-latest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := thinking.ParseSuffix(tt.model)
			if result.ModelName != tt.wantBase {
				t.Errorf("ParseSuffix(%q).ModelName = %q, want %q", tt.model, result.ModelName, tt.wantBase)
			}
		})
	}
}

func TestEnsureQwenSystemMessage_MergeStringSystem(t *testing.T) {
	payload := []byte(`{
		"model": "qwen3.6-plus",
		"stream": true,
		"messages": [
			{ "role": "system", "content": "ABCDEFG" },
			{ "role": "user", "content": [ { "type": "text", "text": "你好" } ] }
		]
	}`)

	out, err := ensureQwenSystemMessage(payload)
	if err != nil {
		t.Fatalf("ensureQwenSystemMessage() error = %v", err)
	}

	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("messages length = %d, want 2", len(msgs))
	}
	if msgs[0].Get("role").String() != "system" {
		t.Fatalf("messages[0].role = %q, want %q", msgs[0].Get("role").String(), "system")
	}
	parts := msgs[0].Get("content").Array()
	if len(parts) != 2 {
		t.Fatalf("messages[0].content length = %d, want 2", len(parts))
	}
	if parts[0].Get("type").String() != "text" || parts[0].Get("cache_control.type").String() != "ephemeral" {
		t.Fatalf("messages[0].content[0] = %s, want injected system part", parts[0].Raw)
	}
	if text := parts[0].Get("text").String(); text != "" && text != "You are Qwen Code." {
		t.Fatalf("messages[0].content[0].text = %q, want empty string or default prompt", text)
	}
	if parts[1].Get("type").String() != "text" || parts[1].Get("text").String() != "ABCDEFG" {
		t.Fatalf("messages[0].content[1] = %s, want text part with ABCDEFG", parts[1].Raw)
	}
}

func TestEnsureQwenSystemMessage_PrependsWhenMissing(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{ "role": "user", "content": [ { "type": "text", "text": "你好" } ] }
		]
	}`)

	out, err := ensureQwenSystemMessage(payload)
	if err != nil {
		t.Fatalf("ensureQwenSystemMessage() error = %v", err)
	}

	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("messages length = %d, want 2", len(msgs))
	}
	if msgs[0].Get("role").String() != "system" {
		t.Fatalf("messages[0].role = %q, want %q", msgs[0].Get("role").String(), "system")
	}
	if !msgs[0].Get("content").IsArray() || len(msgs[0].Get("content").Array()) == 0 {
		t.Fatalf("messages[0].content = %s, want non-empty array", msgs[0].Get("content").Raw)
	}
}

func TestWrapQwenError_ExplicitFreeTierQuotaMapsTo429(t *testing.T) {
	body := []byte(`{"error":{"code":"insufficient_quota","message":"Free allocated quota exceeded.","type":"insufficient_quota"}}`)
	code, retryAfter := wrapQwenError(context.Background(), http.StatusTooManyRequests, body)
	if code != http.StatusTooManyRequests {
		t.Fatalf("wrapQwenError status = %d, want %d", code, http.StatusTooManyRequests)
	}
	if retryAfter != nil {
		t.Fatalf("wrapQwenError retryAfter = %v, want nil", *retryAfter)
	}
}

func TestWrapQwenError_DoesNotReclassifyPaidQuotaMessage(t *testing.T) {
	body := []byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota"}}`)
	code, retryAfter := wrapQwenError(context.Background(), http.StatusForbidden, body)
	if code != http.StatusForbidden {
		t.Fatalf("wrapQwenError status = %d, want %d", code, http.StatusForbidden)
	}
	if retryAfter != nil {
		t.Fatalf("wrapQwenError retryAfter = %v, want nil", *retryAfter)
	}
}

func TestQwenExecutorExecuteUsesOpenAICompatibleChatCompletionsForQwenModel(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody []byte
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-standard",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen3.6-plus",
		Payload: []byte(`{"model":"qwen3.6-plus","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/chat/completions")
	}
	if gotAuth != "Bearer access-token" {
		t.Fatalf("authorization = %q, want %q", gotAuth, "Bearer access-token")
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "qwen3.6-plus" {
		t.Fatalf("model = %q, want %q", got, "qwen3.6-plus")
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want %q", got, "ok")
	}
}

func TestQwenExecutorExecuteCoderModelUsesOpenAICompatibleChatCompletions(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotDashScopeAuthType string
	var gotStainlessLang string
	var gotBody []byte
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotDashScopeAuthType = r.Header.Get("X-DashScope-AuthType")
		gotStainlessLang = r.Header.Get("X-Stainless-Lang")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-coder-model",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "coder-model",
		Payload: []byte(`{"model":"coder-model","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/chat/completions")
	}
	if gotAuth != "Bearer access-token" {
		t.Fatalf("authorization = %q, want %q", gotAuth, "Bearer access-token")
	}
	if gotDashScopeAuthType != "qwen-oauth" {
		t.Fatalf("X-DashScope-AuthType = %q, want %q", gotDashScopeAuthType, "qwen-oauth")
	}
	if gotStainlessLang != "" {
		t.Fatalf("X-Stainless-Lang = %q, want empty", gotStainlessLang)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "coder-model" {
		t.Fatalf("model = %q, want %q", got, "coder-model")
	}
	if got := gjson.GetBytes(gotBody, "metadata.channel").String(); got != "cli" {
		t.Fatalf("metadata.channel = %q, want %q", got, "cli")
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want %q", got, "ok")
	}
}

func TestQwenExecutorExecuteCoderModelPrefersDynamicCoderPlusUpstreamModel(t *testing.T) {
	var gotBody []byte
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-dynamic-coder-plus",
		Metadata: map[string]any{
			"access_token": "access-token",
			"qwen_models": []map[string]any{
				{"id": "qwen3.6-plus", "name": "Qwen3.6-Plus"},
				{"id": "qwen3-coder-plus", "name": "Qwen3-Coder"},
			},
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "coder-model",
		Payload: []byte(`{"model":"coder-model","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "qwen3-coder-plus" {
		t.Fatalf("model = %q, want %q", got, "qwen3-coder-plus")
	}
}

func TestQwenExecutorExecuteAddsEphemeralCacheControlToLastArrayMessage(t *testing.T) {
	var gotBody []byte
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-array-cache-control",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "coder-model",
		Payload: []byte(`{"model":"coder-model","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(gotBody, "messages.1.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("messages.1.content.0.cache_control.type = %q, want %q", got, "ephemeral")
	}
}

func TestQwenExecutorExecuteReturnsErrorWhenSetModelFails(t *testing.T) {
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-invalid-json",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
		Attributes: map[string]string{
			"base_url": "https://example.invalid/v1",
		},
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen3.6-plus",
		Payload: []byte(`[]`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err == nil {
		t.Fatal("Execute() expected error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "set") {
		t.Fatalf("error = %q, want mention set model", err.Error())
	}
}

func TestQwenExecutorExecuteStreamUsesOpenAICompatibleChatCompletionsForQwenModel(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody []byte
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-standard-stream",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen3.6-plus",
		Payload: []byte(`{"model":"qwen3.6-plus","messages":[{"role":"user","content":"hello"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/chat/completions")
	}
	if gotAuth != "Bearer access-token" {
		t.Fatalf("authorization = %q, want %q", gotAuth, "Bearer access-token")
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "qwen3.6-plus" {
		t.Fatalf("model = %q, want %q", got, "qwen3.6-plus")
	}

	payloads, errs := drainStreamChunks(t, result.Chunks, 2*time.Second)
	if len(errs) != 0 {
		t.Fatalf("unexpected stream errors: %v", errs)
	}
	if len(payloads) != 2 {
		t.Fatalf("payload count = %d, want 2", len(payloads))
	}
	if !strings.Contains(payloads[0], "\"choices\"") {
		t.Fatalf("first payload = %q, want normalized openai chunk", payloads[0])
	}
	if payloads[1] != "[DONE]" {
		t.Fatalf("last payload = %q, want %q", payloads[1], "[DONE]")
	}
}

func TestQwenExecutorExecuteStreamTranslatesClaudeChunksFromNormalizedOpenAIEvents(t *testing.T) {
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"qwen3-coder-plus\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"index\":0}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"qwen3-coder-plus\",\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"index\":0}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-claude-stream",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
	}

	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen3-coder-plus",
		Payload: []byte(`{"model":"qwen3-coder-plus","max_tokens":128,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: []byte(`{"model":"qwen3-coder-plus","max_tokens":128,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	payloads, errs := drainStreamChunks(t, result.Chunks, 2*time.Second)
	if len(errs) != 0 {
		t.Fatalf("unexpected stream errors: %v", errs)
	}
	if len(payloads) == 0 {
		t.Fatal("payload count = 0, want translated claude events")
	}
	if !strings.Contains(payloads[0], "\"type\":\"message_start\"") {
		t.Fatalf("first payload = %q, want message_start event", payloads[0])
	}
	joined := strings.Join(payloads, "\n")
	if !strings.Contains(joined, "\"type\":\"content_block_delta\"") || !strings.Contains(joined, "\"text\":\"Hello\"") {
		t.Fatalf("joined payloads = %q, want translated text delta", joined)
	}
}

func TestQwenExecutorExecuteStreamReturnsErrorWhenSetModelFails(t *testing.T) {
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-stream-invalid-json",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
		Attributes: map[string]string{
			"base_url": "https://example.invalid/v1",
		},
	}

	_, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen3.6-plus",
		Payload: []byte(`[]`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err == nil {
		t.Fatal("ExecuteStream() expected error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "set") {
		t.Fatalf("error = %q, want mention set model", err.Error())
	}
}

type recordingRoundTripper struct {
	req   *http.Request
	resps []*http.Response
	idx   int
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.req = req.Clone(req.Context())
	if r.idx >= len(r.resps) {
		return nil, errors.New("unexpected request")
	}
	resp := r.resps[r.idx]
	r.idx++
	return resp, nil
}

type errorAfterReader struct {
	data      []byte
	offset    int
	injectErr error
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := copy(p, r.data[r.offset:])
		r.offset += n
		return n, nil
	}
	if r.injectErr != nil {
		err := r.injectErr
		r.injectErr = nil
		return 0, err
	}
	return 0, io.EOF
}

func (r *errorAfterReader) Close() error {
	return nil
}

func TestQwenExecutorExecuteStreamDoesNotSendDoneOnScannerError(t *testing.T) {
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	rt := &recordingRoundTripper{
		resps: []*http.Response{
			{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: &errorAfterReader{
					data:      []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"),
					injectErr: errors.New("scanner boom"),
				},
			},
		},
	}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-stream-scan-error",
		Metadata: map[string]any{
			"access_token": "access-token",
		},
		Attributes: map[string]string{
			"base_url": "https://example.invalid/v1",
		},
	}

	result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "qwen3.6-plus",
		Payload: []byte(`{"model":"qwen3.6-plus","messages":[{"role":"user","content":"hello"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result == nil {
		t.Fatal("ExecuteStream() returned nil result")
	}

	payloads, errs := drainStreamChunks(t, result.Chunks, 2*time.Second)
	if len(errs) == 0 {
		t.Fatalf("expected at least one error chunk, got payloads=%v", payloads)
	}
	for _, p := range payloads {
		if strings.Contains(p, "[DONE]") {
			t.Fatalf("unexpected [DONE] in payloads when scanner error occurs: %v", payloads)
		}
	}
}

func TestQwenExecutorExecuteRefreshesAndRetriesUnauthorizedOnce(t *testing.T) {
	var requests int
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	recorder := &qwenRefreshRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		authHeader := r.Header.Get("Authorization")
		switch requests {
		case 1:
			if authHeader != "Bearer expired-token" {
				t.Fatalf("first authorization = %q, want %q", authHeader, "Bearer expired-token")
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"invalid access token or token expired","type":"invalid_request_error"}}`))
		case 2:
			if authHeader != "Bearer refreshed-token" {
				t.Fatalf("second authorization = %q, want %q", authHeader, "Bearer refreshed-token")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			t.Fatalf("unexpected request count: %d", requests)
		}
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-refresh-inline",
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-token",
			"type":          "qwen",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
		Runtime: recorder,
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "coder-model",
		Payload: []byte(`{"model":"coder-model","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if recorder.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", recorder.calls)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want %q", got, "ok")
	}
}

func TestQwenExecutorExecuteStreamRefreshesAndRetriesUnauthorizedOnce(t *testing.T) {
	var requests int
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	recorder := &qwenRefreshRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		authHeader := r.Header.Get("Authorization")
		switch requests {
		case 1:
			if authHeader != "Bearer expired-token" {
				t.Fatalf("first authorization = %q, want %q", authHeader, "Bearer expired-token")
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"session_expired","message":"session expired","type":"auth_error"}}`))
		case 2:
			if authHeader != "Bearer refreshed-token" {
				t.Fatalf("second authorization = %q, want %q", authHeader, "Bearer refreshed-token")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			t.Fatalf("unexpected request count: %d", requests)
		}
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-stream-refresh-inline",
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-token",
			"type":          "qwen",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
		Runtime: recorder,
	}

	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "coder-model",
		Payload: []byte(`{"model":"coder-model","messages":[{"role":"user","content":"hello"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if recorder.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", recorder.calls)
	}
	payloads, errs := drainStreamChunks(t, result.Chunks, 2*time.Second)
	if len(errs) != 0 {
		t.Fatalf("unexpected stream errors: %v", errs)
	}
	if len(payloads) != 2 {
		t.Fatalf("payload count = %d, want 2", len(payloads))
	}
}

func TestQwenExecutorExecuteDoesNotRefreshQuotaError(t *testing.T) {
	var requests int
	clearQwenRateLimiter()
	t.Cleanup(clearQwenRateLimiter)

	recorder := &qwenRefreshRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota"}}`))
	}))
	defer server.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "qwen-auth-no-refresh-quota",
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "refresh-token",
			"type":          "qwen",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/v1",
		},
		Runtime: recorder,
	}

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "coder-model",
		Payload: []byte(`{"model":"coder-model","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err == nil {
		t.Fatal("Execute() expected error, got nil")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if recorder.calls != 0 {
		t.Fatalf("refresh calls = %d, want 0", recorder.calls)
	}
}

func drainStreamChunks(t *testing.T, ch <-chan cliproxyexecutor.StreamChunk, timeout time.Duration) (payloads []string, errs []error) {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return payloads, errs
			}
			if chunk.Err != nil {
				errs = append(errs, chunk.Err)
				continue
			}
			payloads = append(payloads, string(chunk.Payload))
		case <-timer.C:
			t.Fatalf("timed out waiting for stream chunks")
		}
	}
}
