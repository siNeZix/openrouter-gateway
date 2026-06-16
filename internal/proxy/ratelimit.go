package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openrouter-gateway/internal/keys"
)

// ParseRateLimits extracts remaining limit and rate limit headers to lazily update key state
func ParseRateLimits(ks *keys.KeyState, headers http.Header) {
	// OpenRouter returns standard rate limit headers:
	// X-RateLimit-Limit: max requests
	// X-RateLimit-Remaining: remaining requests
	// X-RateLimit-Reset: time when limit resets (seconds or Unix timestamp)

	if remStr := headers.Get("X-RateLimit-Remaining"); remStr != "" {
		if rem, err := strconv.ParseInt(remStr, 10, 64); err == nil {
			ks.UpdateLimitRemaining(rem)
		}
	}
}

// HandleProxyError updates key state based on OpenRouter response status and headers.
// Returns the duration of the cooldown applied, if any.
func HandleProxyError(ks *keys.KeyState, resp *http.Response) time.Duration {
	now := time.Now()
	var cooldown time.Duration

	switch resp.StatusCode {
	case http.StatusTooManyRequests: // 429
		cooldown = 2 * time.Minute // default fallback

		// Try to parse Retry-After header (seconds or HTTP-date)
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			if secs, err := strconv.Atoi(retryAfter); err == nil {
				cooldown = time.Duration(secs) * time.Second
			}
		}

		// Also check for X-RateLimit-Reset (seconds remaining until reset)
		if resetStr := resp.Header.Get("X-RateLimit-Reset"); resetStr != "" {
			if resetSecs, err := strconv.ParseFloat(resetStr, 64); err == nil && resetSecs > 0 {
				// Sometimes reset is a Unix timestamp, sometimes seconds.
				// If it is large, treat as Unix timestamp.
				if resetSecs > 1000000000 {
					resetTime := time.Unix(int64(resetSecs), 0)
					if resetTime.After(now) {
						cooldown = resetTime.Sub(now)
					}
				} else {
					cooldown = time.Duration(resetSecs) * time.Second
				}
			}
		}

		// Cap cooldown to 5 minutes to prevent long-term lockouts from brief spikes
		if cooldown > 5*time.Minute {
			cooldown = 5 * time.Minute
		}

		ks.SetCooldown(cooldown, "rate_limited")

	case http.StatusUnauthorized, http.StatusForbidden: // 401, 403
		ks.SetStatus("invalid")

	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 5xx
		cooldown = 30 * time.Second
		ks.SetCooldown(cooldown, "") // Keep status active, just cooldown

	default:
		// Other errors: check if body contains rate-limit or quota exhausted message
		// Read a small preview of body if needed, but usually status codes are sufficient.
	}

	return cooldown
}

// OpenRouterErrorResponse is the structure returned by OpenRouter on error
type OpenRouterErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

func IsQuotaExhaustedError(body []byte) bool {
	var errResp OpenRouterErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil {
		msg := strings.ToLower(errResp.Error.Message)
		if strings.Contains(msg, "credit") || strings.Contains(msg, "quota") || strings.Contains(msg, "exhausted") || strings.Contains(msg, "limit exceeded") {
			return true
		}
	}
	return false
}
