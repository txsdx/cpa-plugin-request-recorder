// Package wire implements the public CPA ABI v1 / JSON schema v6 subset used by
// the recorder. Field casing and []byte base64 encoding match sdk/pluginapi.
// Keeping the wire DTOs local avoids linking the entire CPA server into a plugin.
package wire

import (
	"net/http"
	"net/url"
	"time"
)

type Request struct {
	RequestID, TraceID, SourceFormat, ToFormat, Model, RequestedModel, CallerScope string
	Stream                                                                         bool
	Headers                                                                        http.Header
	Body                                                                           []byte
	Metadata                                                                       map[string]any
}
type Response struct {
	RequestID, SourceFormat, Model, RequestedModel string
	Stream                                         bool
	RequestHeaders, ResponseHeaders                http.Header
	OriginalRequest, RequestBody, Body             []byte
	StatusCode                                     int
	ChunkIndex                                     int
	Metadata                                       map[string]any
}
type Completion struct {
	RequestID, TraceID, SourceFormat, Model, RequestedModel, Outcome, Error string
	Stream                                                                  bool
	StatusCode                                                              int
	StartedAt, CompletedAt                                                  time.Time
	Metadata                                                                map[string]any
}
type Usage struct {
	RequestID, TraceID, Provider, BaseURL, ExecutorType, Model, Alias, APIKey              string
	AccessProvider, SessionID, ParentSessionID, AuthID, AuthIndex, AuthType, Source        string
	ReasoningEffort, ServiceTier, EffectiveServiceTier, ResponseServiceTier, ResponseModel string
	Stream, Failed                                                                         bool
	RequestedAt                                                                            time.Time
	Latency, TTFT                                                                          time.Duration
	Detail                                                                                 Tokens
	Failure                                                                                struct {
		StatusCode int
		Body       string
	}
	ResponseHeaders http.Header
}
type Tokens struct {
	InputTokens, OutputTokens, ReasoningTokens, CachedTokens, CacheReadTokens, CacheCreationTokens, TotalTokens int64
}
type ManagementRequest struct {
	Method, Path string
	Headers      http.Header
	Query        url.Values
	Body         []byte
}
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
