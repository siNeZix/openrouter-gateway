package proxy

import (
	"testing"
)

func TestIsUpstreamRateLimit(t *testing.T) {
	// From real user logs
	upstreamBody := []byte(`{"error":{"message":"Provider returned error","code":429,"metadata":{"raw":"google/gemma-4-31b-it:free is temporarily rate-limited upstream. Retry shortly...","provider_name":"Google AI Studio","is_byok":false}},"user_id":"user_3E7vwYlaMGIuYQMPJ3mmb8JljS9"}`)
	if !IsUpstreamRateLimit(upstreamBody) {
		t.Error("expected true for upstream rate-limit error")
	}

	// BYOK key rate-limit
	byokBody := []byte(`{"error":{"message":"Provider returned error","code":429,"metadata":{"raw":"rate-limited upstream...","provider_name":"Google AI Studio","is_byok":true}}}`)
	if !IsUpstreamRateLimit(byokBody) {
		t.Error("expected true when raw contains rate-limited upstream even with byok")
	}

	// Normal 429 rate limit (key limit, no provider metadata)
	normal429 := []byte(`{"error":{"message":"You have exceeded your request rate. Please try again later.","code":429}}`)
	if IsUpstreamRateLimit(normal429) {
		t.Error("expected false for regular key limit 429")
	}

	// Quota exhausted
	quotaExhausted := []byte(`{"error":{"message":"Credit exhausted","code":402}}`)
	if IsUpstreamRateLimit(quotaExhausted) {
		t.Error("expected false for credit exhausted")
	}
}
