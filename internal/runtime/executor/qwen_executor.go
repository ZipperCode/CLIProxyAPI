package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	qwenauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/qwen"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	qwenUserAgent       = "QwenCode/0.14.2 (darwin; arm64)"
	qwenRateLimitPerMin = 60          // 60 requests per minute per credential
	qwenRateLimitWindow = time.Minute // sliding window duration
	qwenAnonymousAuthID = "__qwen_anonymous__"
)

var qwenDefaultSystemMessage = []byte(`{"role":"system","content":[{"type":"text","text":"","cache_control":{"type":"ephemeral"}}]}`)

type qwenRuntimeRefresher interface {
	Refresh(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error)
}

// qwenQuotaCodes is a package-level set of error codes that indicate quota exhaustion.
var qwenQuotaCodes = map[string]struct{}{
	"insufficient_quota": {},
	"quota_exceeded":     {},
}

// qwenAuthSessionCodes lists error codes that indicate the Qwen session/auth state is invalid.
var qwenAuthSessionCodes = map[string]struct{}{
	"session_expired": {},
	"session_invalid": {},
	"need_login":      {},
}

// qwenRateLimiter tracks request timestamps per credential for rate limiting.
// Qwen has a limit of 60 requests per minute per account.
var qwenRateLimiter = struct {
	sync.Mutex
	requests map[string][]time.Time // authID -> request timestamps
}{
	requests: make(map[string][]time.Time),
}

// redactAuthID returns a redacted version of the auth ID for safe logging.
// Keeps a small prefix/suffix to allow correlation across events.
func redactAuthID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return id[:4] + "..." + id[len(id)-4:]
}

// checkQwenRateLimit checks if the credential has exceeded the rate limit.
// Returns nil if allowed, or a statusErr with retryAfter if rate limited.
func checkQwenRateLimit(authID string) error {
	if authID == "" {
		// Empty authID should not bypass rate limiting in production.
		authID = qwenAnonymousAuthID
		log.Warn("qwen rate limit check: empty authID, falling back to anonymous bucket")
	}

	now := time.Now()
	windowStart := now.Add(-qwenRateLimitWindow)

	qwenRateLimiter.Lock()
	defer qwenRateLimiter.Unlock()

	// Get and filter timestamps within the window
	timestamps := qwenRateLimiter.requests[authID]
	var validTimestamps []time.Time
	for _, ts := range timestamps {
		if ts.After(windowStart) {
			validTimestamps = append(validTimestamps, ts)
		}
	}

	// Always prune expired entries to prevent memory leak
	// Delete empty entries, otherwise update with pruned slice
	if len(validTimestamps) == 0 {
		delete(qwenRateLimiter.requests, authID)
	} else {
		qwenRateLimiter.requests[authID] = validTimestamps
	}

	// Check if rate limit exceeded
	if len(validTimestamps) >= qwenRateLimitPerMin {
		// Calculate when the oldest request will expire
		oldestInWindow := validTimestamps[0]
		retryAfter := oldestInWindow.Add(qwenRateLimitWindow).Sub(now)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		retryAfterSec := int(retryAfter.Seconds())
		return statusErr{
			code:       http.StatusTooManyRequests,
			msg:        fmt.Sprintf(`{"error":{"code":"rate_limit_exceeded","message":"Qwen rate limit: %d requests/minute exceeded, retry after %ds","type":"rate_limit_exceeded"}}`, qwenRateLimitPerMin, retryAfterSec),
			retryAfter: &retryAfter,
		}
	}

	// Record this request and update the map with pruned timestamps
	validTimestamps = append(validTimestamps, now)
	qwenRateLimiter.requests[authID] = validTimestamps

	return nil
}

// isQwenQuotaError checks if the error response indicates a quota exceeded error.
// Qwen returns HTTP 403 with error.code="insufficient_quota" when daily quota is exhausted.
func isQwenQuotaError(body []byte) bool {
	code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	return code == "insufficient_quota" && strings.Contains(msg, "free allocated quota exceeded")
}

// wrapQwenError wraps an HTTP error response, detecting quota errors and mapping them to 429.
// Returns the appropriate status code and retryAfter duration for statusErr.
// Only checks for quota errors when httpCode is 403 or 429 to avoid false positives.
func wrapQwenError(ctx context.Context, httpCode int, body []byte) (errCode int, retryAfter *time.Duration) {
	errCode = httpCode
	// Only check quota errors for expected status codes to avoid false positives
	// Qwen returns 403 for quota errors, 429 for rate limits
	if (httpCode == http.StatusForbidden || httpCode == http.StatusTooManyRequests) && isQwenQuotaError(body) {
		errCode = http.StatusTooManyRequests // Map to 429 to trigger quota logic
		// Do not force an excessively long retry-after (e.g. until tomorrow), otherwise
		// the global request-retry scheduler may skip retries due to max-retry-interval.
		helps.LogWithRequestID(ctx).Warnf("qwen quota exceeded (http %d -> %d)", httpCode, errCode)
	} else if httpCode == http.StatusForbidden && isQwenAuthSessionError(body) {
		errCode = http.StatusUnauthorized
		helps.LogWithRequestID(ctx).Warnf("qwen auth/session error (http %d -> %d), treating as unauthorized: %s", httpCode, errCode, helps.SummarizeErrorBody("application/json", body))
	}
	return errCode, retryAfter
}

func isQwenAuthSessionError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	err := gjson.GetBytes(body, "error")
	code := strings.ToLower(err.Get("code").String())
	if _, ok := qwenAuthSessionCodes[code]; ok {
		return true
	}
	errType := strings.ToLower(err.Get("type").String())
	msg := strings.ToLower(err.Get("message").String())
	if errType == "auth_error" && (strings.Contains(msg, "session") || strings.Contains(msg, "login") || strings.Contains(msg, "token")) {
		return true
	}
	if strings.Contains(msg, "session expired") || strings.Contains(msg, "session invalid") {
		return true
	}
	return false
}

func isQwenAuthRetryableStatus(code int, body []byte) bool {
	if code == http.StatusUnauthorized {
		return true
	}
	if code == http.StatusForbidden && isQwenAuthSessionError(body) {
		return true
	}
	return false
}

func (e *QwenExecutor) refreshInlineAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("qwen executor: auth is nil")
	}
	if runtimeRefresher, ok := auth.Runtime.(qwenRuntimeRefresher); ok && runtimeRefresher != nil {
		return runtimeRefresher.Refresh(ctx, auth)
	}
	return e.Refresh(ctx, auth)
}

// ensureQwenSystemMessage ensures the request has a single system message at the beginning.
// It always injects the default system prompt and merges any user-provided system messages
// into the injected system message content to satisfy Qwen's strict message ordering rules.
func ensureQwenSystemMessage(payload []byte) ([]byte, error) {
	isInjectedSystemPart := func(part gjson.Result) bool {
		if !part.Exists() || !part.IsObject() {
			return false
		}
		if !strings.EqualFold(part.Get("type").String(), "text") {
			return false
		}
		if !strings.EqualFold(part.Get("cache_control.type").String(), "ephemeral") {
			return false
		}
		text := part.Get("text").String()
		return text == "" || text == "You are Qwen Code."
	}

	defaultParts := gjson.ParseBytes(qwenDefaultSystemMessage).Get("content")
	var systemParts []any
	if defaultParts.Exists() && defaultParts.IsArray() {
		for _, part := range defaultParts.Array() {
			systemParts = append(systemParts, part.Value())
		}
	}
	if len(systemParts) == 0 {
		systemParts = append(systemParts, map[string]any{
			"type": "text",
			"text": "You are Qwen Code.",
			"cache_control": map[string]any{
				"type": "ephemeral",
			},
		})
	}

	appendSystemContent := func(content gjson.Result) {
		makeTextPart := func(text string) map[string]any {
			return map[string]any{
				"type": "text",
				"text": text,
			}
		}

		if !content.Exists() || content.Type == gjson.Null {
			return
		}
		if content.IsArray() {
			for _, part := range content.Array() {
				if part.Type == gjson.String {
					systemParts = append(systemParts, makeTextPart(part.String()))
					continue
				}
				if isInjectedSystemPart(part) {
					continue
				}
				systemParts = append(systemParts, part.Value())
			}
			return
		}
		if content.Type == gjson.String {
			systemParts = append(systemParts, makeTextPart(content.String()))
			return
		}
		if content.IsObject() {
			if isInjectedSystemPart(content) {
				return
			}
			systemParts = append(systemParts, content.Value())
			return
		}
		systemParts = append(systemParts, makeTextPart(content.String()))
	}

	messages := gjson.GetBytes(payload, "messages")
	var nonSystemMessages []any
	if messages.Exists() && messages.IsArray() {
		for _, msg := range messages.Array() {
			if strings.EqualFold(msg.Get("role").String(), "system") {
				appendSystemContent(msg.Get("content"))
				continue
			}
			nonSystemMessages = append(nonSystemMessages, msg.Value())
		}
	}

	newMessages := make([]any, 0, 1+len(nonSystemMessages))
	newMessages = append(newMessages, map[string]any{
		"role":    "system",
		"content": systemParts,
	})
	newMessages = append(newMessages, nonSystemMessages...)

	updated, errSet := sjson.SetBytes(payload, "messages", newMessages)
	if errSet != nil {
		return nil, fmt.Errorf("qwen executor: set system message failed: %w", errSet)
	}
	return updated, nil
}

// QwenExecutor is a stateless executor for Qwen Code using OpenAI-compatible chat completions.
// If access token is unavailable, it falls back to legacy via ClientAdapter.
type QwenExecutor struct {
	cfg *config.Config
}

func NewQwenExecutor(cfg *config.Config) *QwenExecutor { return &QwenExecutor{cfg: cfg} }

func (e *QwenExecutor) Identifier() string { return "qwen" }

// PrepareRequest injects Qwen credentials into the outgoing HTTP request.
func (e *QwenExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := qwenCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// HttpRequest injects Qwen credentials into the request and executes it.
func (e *QwenExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("qwen executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *QwenExecutor) executeLegacy(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string) (resp cliproxyexecutor.Response, err error) {
	var authID string
	if auth != nil {
		authID = auth.ID
	}
	if err := checkQwenRateLimit(authID); err != nil {
		helps.LogWithRequestID(ctx).Warnf("qwen rate limit exceeded for credential %s", redactAuthID(authID))
		return resp, err
	}

	token, baseURL := qwenCreds(auth)
	if baseURL == "" {
		baseURL = "https://portal.qwen.ai/v1"
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return resp, fmt.Errorf("qwen executor: set legacy model failed: %w", err)
	}

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, err = ensureQwenSystemMessage(body)
	if err != nil {
		return resp, err
	}
	body, err = ensureQwenMetadata(body)
	if err != nil {
		return resp, err
	}
	body, err = ensureQwenLastMessageCacheControl(body)
	if err != nil {
		return resp, err
	}

	var authLabel, authType, authValue string
	if auth != nil {
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	var data []byte
	var respHeaders http.Header
	currentAuth := auth
	currentToken := token
	currentBaseURL := baseURL
	for attempt := 0; attempt < 2; attempt++ {
		currentURL := strings.TrimSuffix(currentBaseURL, "/") + "/chat/completions"
		currentReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, currentURL, bytes.NewReader(body))
		if reqErr != nil {
			return resp, reqErr
		}
		applyQwenHeaders(currentReq, currentToken, false)
		qwenApplyAuthCustomHeaders(currentReq, currentAuth)
		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       currentURL,
			Method:    http.MethodPost,
			Headers:   currentReq.Header.Clone(),
			Body:      body,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, currentAuth, 0)
		httpResp, doErr := httpClient.Do(currentReq)
		if doErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, doErr)
			return resp, doErr
		}
		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		respBody, readErr := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			return resp, readErr
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, respBody)
		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			data = respBody
			respHeaders = httpResp.Header.Clone()
			break
		}

		if attempt == 0 && isQwenAuthRetryableStatus(httpResp.StatusCode, respBody) {
			updatedAuth, refreshErr := e.refreshInlineAuth(ctx, currentAuth)
			if refreshErr == nil && updatedAuth != nil {
				currentAuth = updatedAuth
				auth = updatedAuth
				currentToken, currentBaseURL = qwenCreds(updatedAuth)
				if currentBaseURL == "" {
					currentBaseURL = "https://portal.qwen.ai/v1"
				}
				continue
			}
		}

		errCode, retryAfter := wrapQwenError(ctx, httpResp.StatusCode, respBody)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d (mapped: %d), error message: %s", httpResp.StatusCode, errCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), respBody))
		err = statusErr{code: errCode, msg: string(respBody), retryAfter: retryAfter}
		return resp, err
	}

	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: respHeaders}
	return resp, nil
}

func (e *QwenExecutor) executeStreamLegacy(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string) (_ *cliproxyexecutor.StreamResult, err error) {
	var authID string
	if auth != nil {
		authID = auth.ID
	}
	if err := checkQwenRateLimit(authID); err != nil {
		helps.LogWithRequestID(ctx).Warnf("qwen rate limit exceeded for credential %s", redactAuthID(authID))
		return nil, err
	}

	token, baseURL := qwenCreds(auth)
	if baseURL == "" {
		baseURL = "https://portal.qwen.ai/v1"
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, err = sjson.SetBytes(body, "model", baseModel)
	if err != nil {
		return nil, fmt.Errorf("qwen executor: set legacy stream model failed: %w", err)
	}

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return nil, fmt.Errorf("qwen executor: set legacy stream usage option failed: %w", err)
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	body = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, err = ensureQwenSystemMessage(body)
	if err != nil {
		return nil, err
	}
	body, err = ensureQwenMetadata(body)
	if err != nil {
		return nil, err
	}
	body, err = ensureQwenLastMessageCacheControl(body)
	if err != nil {
		return nil, err
	}

	var authLabel, authType, authValue string
	if auth != nil {
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	currentAuth := auth
	currentToken := token
	currentBaseURL := baseURL
	var httpResp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		currentURL := strings.TrimSuffix(currentBaseURL, "/") + "/chat/completions"
		currentReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, currentURL, bytes.NewReader(body))
		if reqErr != nil {
			return nil, reqErr
		}
		applyQwenHeaders(currentReq, currentToken, true)
		qwenApplyAuthCustomHeaders(currentReq, currentAuth)
		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       currentURL,
			Method:    http.MethodPost,
			Headers:   currentReq.Header.Clone(),
			Body:      body,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, currentAuth, 0)
		httpResp, err = httpClient.Do(currentReq)
		if err != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, err)
			return nil, err
		}
		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			break
		}
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close response body error: %v", errClose)
		}

		if attempt == 0 && isQwenAuthRetryableStatus(httpResp.StatusCode, b) {
			updatedAuth, refreshErr := e.refreshInlineAuth(ctx, currentAuth)
			if refreshErr == nil && updatedAuth != nil {
				currentAuth = updatedAuth
				auth = updatedAuth
				currentToken, currentBaseURL = qwenCreds(updatedAuth)
				if currentBaseURL == "" {
					currentBaseURL = "https://portal.qwen.ai/v1"
				}
				continue
			}
		}

		errCode, retryAfter := wrapQwenError(ctx, httpResp.StatusCode, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d (mapped: %d), error message: %s", httpResp.StatusCode, errCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: errCode, msg: string(b), retryAfter: retryAfter}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("qwen executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
				continue
			}
			normalized := bytesTrimDataPrefix(line)
			if len(normalized) == 0 {
				continue
			}
			if bytes.Equal(normalized, []byte("[DONE]")) {
				if from.String() == "openai" {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte("[DONE]")}
					continue
				}
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("data: [DONE]"), &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
				}
				continue
			}
			if from.String() == "openai" {
				out <- cliproxyexecutor.StreamChunk{Payload: normalized}
				continue
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, line, &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *QwenExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	baseModel = resolveQwenUpstreamModel(auth, baseModel)
	return e.executeLegacy(ctx, auth, req, opts, baseModel)
}

func qwenApplyAuthCustomHeaders(req *http.Request, auth *cliproxyauth.Auth) {
	if req == nil {
		return
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
}

func bytesTrimDataPrefix(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 {
		return nil
	}
	return trimmed
}

func (e *QwenExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	baseModel = resolveQwenUpstreamModel(auth, baseModel)
	return e.executeStreamLegacy(ctx, auth, req, opts, baseModel)
}

func (e *QwenExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	baseModel = resolveQwenUpstreamModel(auth, baseModel)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelName := gjson.GetBytes(body, "model").String()
	if strings.TrimSpace(modelName) == "" {
		modelName = baseModel
	}

	enc, err := helps.TokenizerForModel(modelName)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qwen executor: tokenizer init failed: %w", err)
	}

	count, err := helps.CountOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qwen executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func (e *QwenExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("qwen executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("qwen executor: auth is nil")
	}
	// Expect refresh_token in metadata for OAuth-based accounts
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		// Nothing to refresh
		return auth, nil
	}

	svc := qwenauth.NewQwenAuth(e.cfg)
	td, err := svc.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.ResourceURL != "" {
		auth.Metadata["resource_url"] = td.ResourceURL
	}
	// Use "expired" for consistency with existing file format
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "qwen"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func applyQwenHeaders(r *http.Request, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("User-Agent", qwenUserAgent)
	r.Header["X-DashScope-UserAgent"] = []string{qwenUserAgent}
	r.Header["X-DashScope-CacheControl"] = []string{"enable"}
	r.Header["X-DashScope-AuthType"] = []string{"qwen-oauth"}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

func ensureQwenMetadata(payload []byte) ([]byte, error) {
	if gjson.GetBytes(payload, "metadata").Exists() {
		if channel := strings.TrimSpace(gjson.GetBytes(payload, "metadata.channel").String()); channel != "" {
			return payload, nil
		}
	}

	out, err := sjson.SetBytes(payload, "metadata.channel", "cli")
	if err != nil {
		return nil, fmt.Errorf("qwen executor: set metadata.channel failed: %w", err)
	}
	return out, nil
}

func ensureQwenLastMessageCacheControl(payload []byte) ([]byte, error) {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload, nil
	}
	items := messages.Array()
	if len(items) == 0 {
		return payload, nil
	}

	lastIndex := len(items) - 1
	lastContent := items[lastIndex].Get("content")
	if !lastContent.Exists() || !lastContent.IsArray() {
		return payload, nil
	}
	parts := lastContent.Array()
	if len(parts) == 0 {
		return payload, nil
	}

	lastTextIndex := -1
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.EqualFold(parts[i].Get("type").String(), "text") {
			lastTextIndex = i
			break
		}
	}
	if lastTextIndex < 0 {
		return payload, nil
	}
	if strings.TrimSpace(parts[lastTextIndex].Get("cache_control.type").String()) != "" {
		return payload, nil
	}

	path := fmt.Sprintf("messages.%d.content.%d.cache_control.type", lastIndex, lastTextIndex)
	out, err := sjson.SetBytes(payload, path, "ephemeral")
	if err != nil {
		return nil, fmt.Errorf("qwen executor: set last message cache_control failed: %w", err)
	}
	return out, nil
}

func normaliseQwenBaseURL(resourceURL string) string {
	raw := strings.TrimSpace(resourceURL)
	if raw == "" {
		return ""
	}

	normalized := raw
	lower := strings.ToLower(normalized)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		normalized = "https://" + normalized
	}

	normalized = strings.TrimRight(normalized, "/")
	if !strings.HasSuffix(strings.ToLower(normalized), "/v1") {
		normalized += "/v1"
	}

	return normalized
}

func resolveQwenUpstreamModel(auth *cliproxyauth.Auth, requestedModel string) string {
	model := strings.TrimSpace(requestedModel)
	if !strings.EqualFold(model, "coder-model") || auth == nil || auth.Metadata == nil {
		return model
	}

	raw, ok := auth.Metadata["qwen_models"]
	if !ok || raw == nil {
		return model
	}

	var items []any
	switch val := raw.(type) {
	case []any:
		items = val
	case []map[string]any:
		items = make([]any, 0, len(val))
		for i := range val {
			items = append(items, val[i])
		}
	default:
		return model
	}

	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok || entry == nil {
			continue
		}
		id, _ := entry["id"].(string)
		if strings.EqualFold(strings.TrimSpace(id), "qwen3-coder-plus") {
			return "qwen3-coder-plus"
		}
	}

	return model
}

func qwenCreds(a *cliproxyauth.Auth) (token, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			token = v
		}
		if v := a.Attributes["base_url"]; v != "" {
			baseURL = v
		}
	}
	if token == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			token = v
		}
		if v, ok := a.Metadata["resource_url"].(string); ok {
			baseURL = normaliseQwenBaseURL(v)
		}
	}
	return
}
