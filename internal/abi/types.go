// Package abi holds the wire structs and envelopes of the CLIProxyAPI native
// plugin ABI (ABI 1, JSON schema 6). The structs are copied rather than
// imported so the plugin does not depend on the host module.
//
// source: sdk/pluginapi/types.go, sdk/pluginabi/types.go,
// internal/pluginhost/rpc_schema.go @ e5b5a1cfca354ec80d5c31242ccfb6c0f6b5fbcd
//
// Copyright (c) 2025-2005.9 Luis Pater
// Copyright (c) 2025.9-present Router-For.ME
// MIT License; full text in THIRD_PARTY_NOTICES.md.
package abi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// ABIVersion is the native function-table version this plugin implements.
const ABIVersion = 1

// SchemaVersion is the JSON RPC schema version this plugin implements.
const SchemaVersion = 6

// StreamHeaderInitIndex is the ChunkIndex of the header-only stream call.
const StreamHeaderInitIndex = -1

// Method names handled or called by the plugin.
const (
	MethodRegister              = "plugin.register"
	MethodReconfigure           = "plugin.reconfigure"
	MethodQuiesce               = "plugin.quiesce"
	MethodShutdown              = "plugin.shutdown"
	MethodUsageHandle           = "usage.handle"
	MethodInterceptAfter        = "response.intercept_after"
	MethodInterceptChunk        = "response.intercept_stream_chunk"
	MethodManagementRegister    = "management.register"
	MethodManagementHandle      = "management.handle"
	MethodHostHTTPDo            = "host.http.do"
	MethodHostHTTPDoStream      = "host.http.do_stream"
	MethodHostHTTPStreamRead    = "host.http.stream_read"
	MethodHostHTTPStreamClose   = "host.http.stream_close"
	MethodHostHTTPOperationOpen = "host.http.operation_open"
	MethodHostHTTPCancel        = "host.http.cancel"
	MethodHostLog               = "host.log"
)

// LifecycleRequest is the payload of plugin.register and plugin.reconfigure.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion int    `json:"schema_version"`
}

// Metadata describes the plugin. The host rejects registrations with an empty
// Name, Version, Author or GitHubRepository.
type Metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []any
}

// Capabilities lists the plugin surfaces this plugin implements.
type Capabilities struct {
	UsagePlugin               bool `json:"usage_plugin"`
	ResponseInterceptor       bool `json:"response_interceptor"`
	ResponseStreamInterceptor bool `json:"response_stream_interceptor"`
	ManagementAPI             bool `json:"management_api"`
}

// Registration is the result of plugin.register and plugin.reconfigure.
type Registration struct {
	SchemaVersion int          `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

// UsageRecord is the usage.handle payload. Durations are nanoseconds.
type UsageRecord struct {
	RequestID           string
	TraceID             string
	Provider            string
	BaseURL             string
	ExecutorType        string
	Model               string
	Alias               string
	APIKey              string
	SessionID           string
	ParentSessionID     string
	AuthID              string
	AuthIndex           string
	AuthType            string
	Source              string
	ReasoningEffort     string
	ServiceTier         string
	ResponseServiceTier string
	ResponseModel       string
	Generate            bool
	Stream              bool
	RequestedAt         time.Time
	Latency             time.Duration
	TTFT                time.Duration
	Failed              bool
	Failure             UsageFailure
	Detail              UsageDetail
	ResponseHeaders     http.Header
}

// UsageFailure describes an upstream or executor failure.
type UsageFailure struct {
	StatusCode int
	Body       string
}

// UsageDetail contains the token counters CPA forwards to usage plugins.
type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// ResponseInterceptRequest is the response.intercept_after payload.
type ResponseInterceptRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	StatusCode      int
	Metadata        map[string]any
}

// ResponseInterceptResponse returns non-streaming response modifications.
// An empty Body keeps the original body.
type ResponseInterceptResponse struct {
	Headers http.Header `json:",omitempty"`
	Body    []byte      `json:",omitempty"`
}

// StreamChunkInterceptRequest is the response.intercept_stream_chunk payload.
// At schema 6 payload chunks omit OriginalRequest, RequestBody and HistoryChunks.
type StreamChunkInterceptRequest struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestedModel  string
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	HistoryChunks   [][]byte
	ChunkIndex      int
	Metadata        map[string]any
}

// StreamChunkInterceptResponse returns stream chunk modifications.
type StreamChunkInterceptResponse struct {
	Headers http.Header `json:",omitempty"`
	Body    []byte      `json:",omitempty"`
}

// ManagementRoute is one plugin-owned route under /v0/management.
type ManagementRoute struct {
	Method      string
	Path        string
	Menu        string `json:",omitempty"`
	Description string `json:",omitempty"`
}

// ResourceRoute is one plugin-owned GET route under /v0/resource/plugins/<id>.
type ResourceRoute struct {
	Path        string
	Menu        string `json:",omitempty"`
	Description string `json:",omitempty"`
}

// ManagementRegistration is the management.register result.
type ManagementRegistration struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// ManagementRequest is the management.handle payload for both route kinds.
type ManagementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

// ManagementResponse is the management.handle result.
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// HostHTTPRequest is the host.http.do payload.
type HostHTTPRequest struct {
	OperationID string              `json:"operation_id,omitempty"`
	Method      string              `json:"method"`
	URL         string              `json:"url"`
	Headers     map[string][]string `json:"headers,omitempty"`
}

// HostHTTPResponse is the host.http.do result.
type HostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// HostLogRequest is the host.log payload.
type HostLogRequest struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// Error is the error member of an envelope.
type Error struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// Envelope wraps every RPC result.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}
