package basispoints

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 0.2.2-pro.5：上传文件名/格式按字节签名（对齐上游 v0.2.4 #15）、
// 图片错误的位置诊断（本地 input[i].content[j] / 上游 image_refs）、429 的 Retry-After 提示。

func encodedImage(t *testing.T, format string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var err error
	switch format {
	case "jpeg":
		err = jpeg.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	case "png":
		err = png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	case "gif":
		err = gif.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	default:
		t.Fatalf("unknown format %q", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// 上传的文件名与 Content-Type 一律取字节签名：声明的 MIME 是别名（image/jpg）不能变成无后缀
// 文件名，带 /etc/mime.types 的环境也不能把 image/jpeg 变成 image.jfif —— 上游按后缀白名单
// 校验，名字写错整条请求都 400（上游 v0.2.4 issue #15 的真实报错）。
func TestUploadFilenameFollowsByteSignature(t *testing.T) {
	pngBytes := encodedImage(t, "png")
	jpegBytes := encodedImage(t, "jpeg")
	gifBytes := encodedImage(t, "gif")
	for _, tc := range []struct {
		name          string
		declared      string
		data          []byte
		wantFile      string
		wantMediaType string
	}{
		{"jpeg-alias", "image/jpg", jpegBytes, "image.jpeg", "image/jpeg"},
		{"mismatched-declaration", "image/png", jpegBytes, "image.jpeg", "image/jpeg"},
		{"png", "image/png", pngBytes, "image.png", "image/png"},
		{"gif", "image/gif", gifBytes, "image.gif", "image/gif"},
		{"webp-without-encoder", "image/webp", []byte("RIFF\x1a\x00\x00\x00WEBPVP8 "), "image.webp", "image/webp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService()
			service.sleep = func(time.Duration) {}
			var uploaded []byte
			service.SetHost(func(method string, payload any, out any) error {
				if method != "host.http.do" {
					return errors.New("unexpected host method " + method)
				}
				uploaded = payload.(map[string]any)["body"].([]byte)
				*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-uploaded"})}
				return nil
			})
			dataURL := "data:" + tc.declared + ";base64," + base64.StdEncoding.EncodeToString(tc.data)
			body := contentBody(imagePart(dataURL))
			if err := service.uploadInputImages(ExecutorRequest{}, body, credential{AccessToken: "token", AccountID: "account"}, defaultConfig()); err != nil {
				t.Fatalf("upload failed: %v", err)
			}
			if !bytes.Contains(uploaded, []byte("filename="+tc.wantFile)) {
				t.Fatalf("multipart filename missing %q", tc.wantFile)
			}
			if !bytes.Contains(uploaded, []byte("Content-Type: "+tc.wantMediaType)) {
				t.Fatalf("multipart content type missing %q", tc.wantMediaType)
			}
		})
	}
}

// 字节签名不在上传白名单内（例如 BMP）时，本地就给出带位置的 400，不把问题留给上游。
func TestUploadRejectsBytesOutsideUploaderWhitelist(t *testing.T) {
	service := NewService()
	service.SetHost(func(string, any, any) error {
		t.Error("unsupported bytes reached the uploader")
		return nil
	})
	bmp := append([]byte("BM"), bytes.Repeat([]byte{0x00}, 62)...)
	body := contentBody(imagePart("data:image/png;base64," + base64.StdEncoding.EncodeToString(bmp)))
	err := service.uploadInputImages(ExecutorRequest{}, body, credential{AccountID: "account"}, defaultConfig())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("err=%v, want 400", err)
	}
	if !strings.Contains(apiErr.Message, "input[0].content[0]") {
		t.Fatalf("missing position diagnostic: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "not a decodable image") && !strings.Contains(apiErr.Message, "supported format") {
		t.Fatalf("unexpected diagnostic: %q", apiErr.Message)
	}
}

// 上游错误里的 image_refs：只给位置与引用类型，不泄漏 URL、file_id 或图片内容。
func TestUpstreamImageRefsDiagnostic(t *testing.T) {
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("x"))
	body := map[string]any{"input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": dataURL, "detail": "original"},
			map[string]any{"type": "input_image", "file_id": "file-abc123"},
		}},
		map[string]any{"type": "function_call_output", "output": []any{
			map[string]any{"type": "input_image", "image_url": "https://example.test/a.png"},
			map[string]any{"type": "input_image"},
		}},
	}}
	err := upstreamRequestError(400, jsonBytes(map[string]any{"error": map[string]any{"message": "boom"}}), body, credential{})
	message := err.Error()
	for _, want := range []string{
		"input_images=4",
		"original_detail_images=1",
		"input[0].content[0]:data_url",
		"input[0].content[1]:file_id",
		"input[1].output[0]:image_url",
		"input[1].output[1]:missing",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("diagnostic missing %q: %s", want, message)
		}
	}
	for _, leak := range []string{"base64,", "example.test", "file-abc123"} {
		if strings.Contains(message, leak) {
			t.Fatalf("diagnostic leaked %q: %s", leak, message)
		}
	}
}

// 429 最终失败时把上游建议的等待时间带进错误正文（CPA 的插件 ABI 没有错误响应头通道）。
func TestRateLimitedErrorCarriesRetryHint(t *testing.T) {
	service := NewService()
	service.sleep = func(time.Duration) {}
	service.SetHost(func(method string, payload any, out any) error {
		*out.(*upstreamResponse) = upstreamResponse{
			StatusCode: 429,
			Headers:    http.Header{"Retry-After": {"4"}},
			Body:       jsonBytes(map[string]any{"error": map[string]any{"message": "slow down"}}),
		}
		return nil
	})
	_, err := service.upstreamRequest(ExecutorRequest{}, map[string]any{"input": []any{}}, credential{AccountID: "account"}, false)
	if err == nil || !strings.Contains(err.Error(), "retry_after=4s") {
		t.Fatalf("err=%v, want retry_after hint", err)
	}
}
