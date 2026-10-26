package recorder

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

type Summary struct {
	LatestPrompt    string       `json:"latest_prompt"`
	LatestResponse  string       `json:"latest_response"`
	ID              string       `json:"id"`
	TraceID         string       `json:"trace_id"`
	StartedAt       time.Time    `json:"started_at"`
	CompletedAt     time.Time    `json:"completed_at"`
	Outcome         string       `json:"outcome"`
	Status          int          `json:"status"`
	Format          string       `json:"format"`
	Stream          bool         `json:"stream"`
	KeyID           string       `json:"key_id"`
	KeyHint         string       `json:"key_hint"`
	KeyAlias        string       `json:"key_alias"`
	CallerScope     string       `json:"caller_scope"`
	ClientIP        string       `json:"client_ip"`
	UserAgent       string       `json:"user_agent"`
	SessionID       string       `json:"session_id"`
	ParentSessionID string       `json:"parent_session_id"`
	Provider        string       `json:"provider"`
	RequestedModel  string       `json:"requested_model"`
	Model           string       `json:"model"`
	ResponseModel   string       `json:"response_model"`
	ReasoningEffort string       `json:"reasoning_effort"`
	ServiceTier     string       `json:"service_tier"`
	AuthID          string       `json:"auth_id"`
	DurationMS      float64      `json:"duration_ms"`
	TTFTMS          *float64     `json:"ttft_ms"`
	FirstChunkMS    *float64     `json:"first_chunk_ms"`
	Tokens          *wire.Tokens `json:"tokens"`
	CacheRatio      *float64     `json:"cache_ratio"`
	UpstreamState   string       `json:"upstream_state"`
	Warnings        []string     `json:"warnings"`
}
type Record struct {
	Summary
	UpstreamAttempt   int            `json:"upstream_attempt"`
	Images            []ImageAsset   `json:"images,omitempty"`
	UsageAttempts     []UsageAttempt `json:"usage_attempts,omitempty"`
	RequestBody       string         `json:"request_body"`
	RequestSource     string         `json:"request_source"`
	InterceptedBody   string         `json:"intercepted_body"`
	RequestHeaders    http.Header    `json:"request_headers"`
	ResponseHeaders   http.Header    `json:"response_headers"`
	ResponseBody      string         `json:"response_body"`
	ResponseSource    string         `json:"response_source"`
	UpstreamRequests  string         `json:"upstream_requests"`
	UpstreamResponses string         `json:"upstream_responses"`
	Error             string         `json:"error"`
	UpstreamError     string         `json:"upstream_error"`
}
type UsageAttempt struct {
	ID               string      `json:"id"`
	Model            string      `json:"model"`
	Provider         string      `json:"provider"`
	AuthID           string      `json:"auth_id"`
	Failed           bool        `json:"failed"`
	RequestedAt      time.Time   `json:"requested_at"`
	DurationMS       float64     `json:"duration_ms"`
	TTFTMS           float64     `json:"ttft_ms"`
	Tokens           wire.Tokens `json:"tokens"`
	InputDenominator int64       `json:"cache_input_denominator"`
}

func fingerprint(key string) string { h := sha256.Sum256([]byte(key)); return hex.EncodeToString(h[:]) }
func keyHint(key string) string {
	if len(key) < 8 {
		return "••••"
	}
	return "••••" + key[len(key)-4:]
}
func safeHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		l := strings.ToLower(k)
		if strings.Contains(l, "authorization") || strings.Contains(l, "cookie") || strings.Contains(l, "key") || strings.Contains(l, "token") || strings.Contains(l, "secret") {
			out[k] = []string{"[REDACTED]"}
		} else {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}
func addWarning(r *Record, s string) {
	for _, v := range r.Warnings {
		if v == s {
			return
		}
	}
	r.Warnings = append(r.Warnings, s)
}
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
