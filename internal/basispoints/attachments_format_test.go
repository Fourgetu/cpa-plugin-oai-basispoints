package basispoints

import (
	"bytes"
	"testing"
)

// 没有注册解码器的格式（例如 webp）不能因为本插件读不出图片头就被判废：
// 体积校验仍然生效，只有"格式/像素"这类需要读头的校验被跳过。
func TestValidateInlineImageSkipsUnknownFormats(t *testing.T) {
	webp := inlineImage{mediaType: "image/webp", data: []byte("RIFF\x1a\x00\x00\x00WEBPVP8 ")}
	if err := validateInlineImage(webp); err != nil {
		t.Fatalf("a format without a registered decoder was rejected: %v", err)
	}
	oversized := inlineImage{mediaType: "image/webp", data: bytes.Repeat([]byte("x"), maxInlineImageBytes+1)}
	if err := validateInlineImage(oversized); err == nil {
		t.Fatal("the decoded size limit was skipped for a format without a decoder")
	}
	// 声明了有解码器的类型、内容却是坏图：仍然要拒（内容与声明不符）。
	broken := inlineImage{mediaType: "image/png", data: []byte("\x89PNG\r\n\x1a\n\x00\x00\x00")}
	if err := validateInlineImage(broken); err == nil {
		t.Fatal("a truncated PNG was accepted")
	}
	// 声明类型与实际格式不一致：也要拒。
	mismatched := inlineImage{mediaType: "image/webp", data: []byte("\x89PNG\r\n\x1a\n")}
	if err := validateInlineImage(mismatched); err == nil {
		t.Fatal("a mismatched media type was accepted")
	}
}
