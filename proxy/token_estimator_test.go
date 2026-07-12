package proxy

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const onePixelPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func imageBlockForTest(data string) map[string]interface{} {
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "image/png",
			"data":       data,
		},
	}
}

func longerEncodingOfTestImage(t *testing.T) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(onePixelPNGBase64)
	if err != nil {
		t.Fatalf("decode test image: %v", err)
	}
	raw = append(raw, bytes.Repeat([]byte{0xa5}, 8192)...)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestEstimateClaudeImageTokensIgnoresBase64Length(t *testing.T) {
	shortTokens := estimateClaudeValueTokens(imageBlockForTest(onePixelPNGBase64))
	longTokens := estimateClaudeValueTokens(imageBlockForTest(longerEncodingOfTestImage(t)))
	if shortTokens != defaultImageInputTokens {
		t.Fatalf("expected minimum image cost %d, got %d", defaultImageInputTokens, shortTokens)
	}
	if longTokens != shortTokens {
		t.Fatalf("same-dimension images must not depend on base64 length: short=%d long=%d", shortTokens, longTokens)
	}
}

func TestEstimateOpenAIImageTokensIncludesFixedImageCost(t *testing.T) {
	build := func(data string) []interface{} {
		return []interface{}{
			map[string]interface{}{"type": "text", "text": "describe this image"},
			map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]interface{}{"url": "data:image/png;base64," + data},
			},
		}
	}

	textTokens := estimateApproxTokens("describe this image")
	shortTokens := estimateOpenAIContentTokens(build(onePixelPNGBase64))
	longTokens := estimateOpenAIContentTokens(build(longerEncodingOfTestImage(t)))
	if shortTokens != textTokens+defaultImageInputTokens {
		t.Fatalf("expected text plus image cost %d, got %d", textTokens+defaultImageInputTokens, shortTokens)
	}
	if longTokens != shortTokens {
		t.Fatalf("same-dimension OpenAI images must not depend on base64 length: short=%d long=%d", shortTokens, longTokens)
	}
}

func TestPromptCacheImageAccountingUsesDigestAndFixedTokens(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	system := []interface{}{map[string]interface{}{
		"type":          "text",
		"text":          strings.Repeat("cacheable system context ", 180),
		"cache_control": map[string]interface{}{"type": "ephemeral"},
	}}
	build := func(data string) *ClaudeRequest {
		return &ClaudeRequest{
			Model:  "claude-sonnet-4.6",
			System: system,
			Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "describe"},
				imageBlockForTest(data),
			}}},
		}
	}

	shortReq := build(onePixelPNGBase64)
	longReq := build(longerEncodingOfTestImage(t))
	shortProfile := tracker.BuildClaudeProfile(shortReq, estimateClaudeRequestInputTokens(shortReq))
	longProfile := tracker.BuildClaudeProfile(longReq, estimateClaudeRequestInputTokens(longReq))
	if shortProfile == nil || longProfile == nil {
		t.Fatal("expected cache profiles for image requests")
	}
	shortLast := shortProfile.Breakpoints[len(shortProfile.Breakpoints)-1]
	longLast := longProfile.Breakpoints[len(longProfile.Breakpoints)-1]
	if shortLast.CumulativeTokens != longLast.CumulativeTokens {
		t.Fatalf("cache token accounting must ignore base64 length: short=%d long=%d", shortLast.CumulativeTokens, longLast.CumulativeTokens)
	}
	if shortLast.Fingerprint == longLast.Fingerprint {
		t.Fatal("different image bytes must produce different cache fingerprints")
	}
}
