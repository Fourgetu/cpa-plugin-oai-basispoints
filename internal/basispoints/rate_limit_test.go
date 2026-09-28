package basispoints

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// 0.2.2-pro.4：限速与上游 5xx 的有界退避、过期附件缓存复用、以及"上游 5xx 不做账号冷却"。

// 429/503/500/502/504 退避两次；Retry-After 只在合理范围内采纳。
func TestRateLimitRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		attempt    int
		wantRetry  bool
		wantDelay  time.Duration
	}{
		{"first 429", 429, "", 0, true, time.Second},
		{"second 429", 429, "", 1, true, 2 * time.Second},
		{"third 429 gives up", 429, "", 2, false, 0},
		{"503 retries", 503, "", 0, true, time.Second},
		{"500 retries", 500, "", 0, true, time.Second},
		{"502 retries", 502, "", 0, true, time.Second},
		{"504 retries", 504, "", 0, true, time.Second},
		{"422 does not retry", 422, "", 0, false, 0},
		{"401 does not retry", 401, "", 0, false, 0},
		{"no status does not retry", 0, "", 0, false, 0},
		{"retry-after honored", 429, "3", 0, true, 3 * time.Second},
		{"retry-after too long gives up", 429, "120", 0, false, 0},
		{"http-date retry-after gives up", 429, "Wed, 21 Oct 2015 07:28:00 GMT", 0, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			if tc.retryAfter != "" {
				headers.Set("Retry-After", tc.retryAfter)
			}
			delay, retry := rateLimitRetryDelay(tc.status, headers, tc.attempt)
			if retry != tc.wantRetry || delay != tc.wantDelay {
				t.Fatalf("delay=%v retry=%v, want %v/%v", delay, retry, tc.wantDelay, tc.wantRetry)
			}
		})
	}
}

// 上游 429 之后重试一次成功：客户端不该看到 429。
func TestUpstreamRetriesRateLimitThenSucceeds(t *testing.T) {
	service := NewService()
	service.sleep = func(time.Duration) {}
	calls := 0
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.http.do" {
			return fmt.Errorf("unexpected host method %s", method)
		}
		calls++
		if calls == 1 {
			*out.(*upstreamResponse) = upstreamResponse{
				StatusCode: 429,
				Headers:    http.Header{"Retry-After": {"1"}},
				Body:       jsonBytes(map[string]any{"error": map[string]any{"message": "slow down"}}),
			}
			return nil
		}
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: jsonBytes(map[string]any{"id": "resp_ok"})}
		return nil
	})
	response, err := service.upstreamRequest(ExecutorRequest{}, map[string]any{"input": []any{}}, credential{AccessToken: "token", AccountID: "account"}, false)
	if err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if calls != 2 || response.StatusCode != 200 {
		t.Fatalf("calls=%d status=%d, want 2/200", calls, response.StatusCode)
	}
}

// 附件上传被限速时同样退避重试，成功即照常回填 file_id。
func TestAttachmentUploadRetriesRateLimitThenSucceeds(t *testing.T) {
	tiny, _ := testImageDataURL(t)
	service := NewService()
	service.sleep = func(time.Duration) {}
	attempts := 0
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.http.do" {
			return fmt.Errorf("unexpected host method %s", method)
		}
		attempts++
		if attempts == 1 {
			*out.(*upstreamResponse) = upstreamResponse{
				StatusCode: 429,
				Headers:    http.Header{"Retry-After": {"2"}},
				Body:       jsonBytes(map[string]any{"error": map[string]any{"message": "File upload was rate limited by OpenAI."}}),
			}
			return nil
		}
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-uploaded"})}
		return nil
	})
	body := contentBody(imagePart(tiny))
	if err := service.uploadInputImages(ExecutorRequest{}, body, credential{AccessToken: "token", AccountID: "account"}, defaultConfig()); err != nil {
		t.Fatalf("upload retry did not recover: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("upload attempts=%d, want 2", attempts)
	}
	part := objectValue(objectValue(body["input"].([]any)[0])["content"].([]any)[0])
	if part["file_id"] != "file-uploaded" || part["image_url"] != nil {
		t.Fatalf("file_id was not written back: %#v", part)
	}
}

// 上游报"附件归属校验失败"：清空附件缓存（下一轮重新上传），且不做无意义的重试。
func TestStaleAttachmentOwnershipClearsAttachmentCache(t *testing.T) {
	service := NewService()
	service.sleep = func(time.Duration) {}
	var key [sha256.Size]byte
	key[0] = 1
	if _, err := service.attachments.getOrUpload(key, 0, func() (string, error) { return "file-old", nil }); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service.SetHost(func(method string, payload any, out any) error {
		calls++
		*out.(*upstreamResponse) = upstreamResponse{
			StatusCode: 500,
			Body:       jsonBytes(map[string]any{"error": map[string]any{"message": "Unknown error while validating file ownership."}}),
		}
		return nil
	})
	if _, err := service.upstreamRequest(ExecutorRequest{}, map[string]any{"input": []any{}}, credential{AccountID: "account"}, false); err == nil {
		t.Fatal("ownership rejection must surface as an error")
	}
	if calls != 1 {
		t.Fatalf("host calls=%d, want 1 (retrying a stale file_id cannot succeed)", calls)
	}
	service.attachments.mu.Lock()
	remaining := len(service.attachments.entries)
	service.attachments.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("attachment cache was not cleared: %d entries", remaining)
	}
}

// 上游 5xx 以内联 response.failed 交付（HTTP 200 SSE）：交给 CPA 会把唯一凭据冷却成 503 墙。
func TestUpstreamServerErrorStreamsInlineFailure(t *testing.T) {
	service := NewService()
	service.sleep = func(time.Duration) {}
	var mu sync.Mutex
	var emitted []string
	closed := make(chan string, 4)
	streamCalls := 0
	service.SetHost(func(method string, payload any, out any) error {
		switch method {
		case "host.http.do_stream":
			streamCalls++
			*out.(*upstreamStream) = upstreamStream{StatusCode: 500, StreamID: "upstream-500", Headers: http.Header{"Content-Type": {"application/json"}}}
			return nil
		case "host.http.stream_read":
			*out.(*streamChunk) = streamChunk{Payload: jsonBytes(map[string]any{"error": map[string]any{"message": "Basis Points internal server error"}}), Done: true}
			return nil
		case "host.http.stream_close":
			return nil
		case "host.stream.emit":
			mu.Lock()
			emitted = append(emitted, string(payload.(map[string]any)["payload"].([]byte)))
			mu.Unlock()
			return nil
		case "host.stream.close":
			raw, _ := json.Marshal(payload)
			var request struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &request)
			select {
			case closed <- request.Error:
			default:
			}
			return nil
		}
		return fmt.Errorf("unexpected host method %s", method)
	})
	result, err := service.Handle("executor.execute_stream", jsonBytes(streamRequest(namespaceTestSource("function", "get_weather", ""))))
	if err != nil {
		t.Fatalf("upstream 5xx was reported as a plugin error (CPA would cool the account): %v", err)
	}
	if result == nil {
		t.Fatal("no stream headers were returned")
	}
	select {
	case closeErr := <-closed:
		if closeErr != "" {
			t.Fatalf("stream closed with an error: %q", closeErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream was not closed")
	}
	mu.Lock()
	joined := strings.Join(emitted, "")
	mu.Unlock()
	if !strings.Contains(joined, "response.failed") || !strings.Contains(joined, "upstream_server_error") {
		t.Fatalf("inline failure was not emitted: %q", joined)
	}
	if streamCalls != rateLimitRetryLimit+1 {
		t.Fatalf("do_stream calls=%d, want %d (bounded retry)", streamCalls, rateLimitRetryLimit+1)
	}
}
