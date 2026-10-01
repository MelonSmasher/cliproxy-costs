// Package api serves the plugin's management and resource routes: the admin
// data family (CPA management auth), the read-token data family and the
// static dashboard assets.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/catalog"
	"github.com/MelonSmasher/cliproxy-costs/internal/config"
	"github.com/MelonSmasher/cliproxy-costs/internal/fx"
	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

// PluginID is the plugin id (library basename) used in resource paths.
const PluginID = "cliproxy-costs"

// Route prefixes.
const (
	AdminPrefix    = "/v0/management/" + PluginID + "/v1/"
	ResourceBase   = "/v0/resource/plugins/" + PluginID
	ReadPrefix     = ResourceBase + "/api/v1/"
	DashboardPath  = ResourceBase + "/dashboard"
	schemaVersion  = 1
	queryTimeout   = 2 * time.Second
	minTokenLength = 32
)

var endpoints = []string{"summary", "quota", "requests", "rates", "fx"}

// View is the immutable per-call state the handlers read.
type View struct {
	Config    *config.Config
	Resolver  *pricing.Resolver
	Feed      catalog.State
	FX        fx.State
	ReadToken *[sha256.Size]byte // nil = read API disabled
	Store     *store.Store       // nil while not running
	Notices   []string
	Now       func() time.Time
}

// ReadTokenHash hashes a configured read token, or returns nil when it is
// shorter than the required minimum (treated as unset).
func ReadTokenHash(token string) *[sha256.Size]byte {
	if len(token) < minTokenLength {
		return nil
	}
	return new(sha256.Sum256([]byte(token)))
}

// Register returns the management.register result.
func Register() abi.ManagementRegistration {
	var reg abi.ManagementRegistration
	for _, ep := range endpoints {
		reg.Routes = append(reg.Routes, abi.ManagementRoute{Method: http.MethodGet, Path: "/" + PluginID + "/v1/" + ep})
		reg.Resources = append(reg.Resources, abi.ResourceRoute{Path: "/api/v1/" + ep, Description: "cliproxy-costs " + ep + " (read token)"})
	}
	reg.Resources = append(reg.Resources, abi.ResourceRoute{Path: "/dashboard", Menu: "Costs", Description: "Usage and cost dashboard"})
	for _, a := range assetPaths {
		reg.Resources = append(reg.Resources, abi.ResourceRoute{Path: "/dashboard/" + a})
	}
	return reg
}

// Handle serves one management.handle request.
func Handle(v *View, req *abi.ManagementRequest) abi.ManagementResponse {
	path := req.Path
	if !strings.EqualFold(req.Method, http.MethodGet) && req.Method != "" {
		return errorResp(http.StatusBadRequest, "bad_request", "only GET is supported")
	}
	switch {
	// Only the exact path serves the page: index.html uses relative URLs
	// (dashboard/app.js, api/v1/…), which break under a trailing slash.
	case path == DashboardPath:
		return serveAsset("")
	case strings.HasPrefix(path, DashboardPath+"/") && path != DashboardPath+"/":
		return serveAsset(strings.TrimPrefix(path, DashboardPath+"/"))
	case strings.HasPrefix(path, ReadPrefix):
		if v.ReadToken == nil {
			return errorResp(http.StatusServiceUnavailable, "read_api_disabled", "read API token is not configured")
		}
		if !checkBearer(req.Headers, v.ReadToken) {
			r := errorResp(http.StatusUnauthorized, "unauthorized", "missing or invalid read token")
			r.Headers.Set("WWW-Authenticate", `Bearer realm="cliproxy-costs"`)
			return r
		}
		return data(v, strings.TrimPrefix(path, ReadPrefix), req, false)
	case strings.HasPrefix(path, AdminPrefix):
		return data(v, strings.TrimPrefix(path, AdminPrefix), req, true)
	}
	return errorResp(http.StatusNotFound, "not_found", "no such route")
}

func checkBearer(h http.Header, want *[sha256.Size]byte) bool {
	var value string
	for k, vs := range h {
		if strings.EqualFold(k, "Authorization") && len(vs) > 0 {
			value = vs[0]
			break
		}
	}
	scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(msg string) error { return &apiError{http.StatusBadRequest, "bad_request", msg} }

func data(v *View, ep string, req *abi.ManagementRequest, admin bool) abi.ManagementResponse {
	if v.Store == nil {
		return errorResp(http.StatusServiceUnavailable, "internal", "plugin storage is not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	var (
		out any
		err error
	)
	q := req.Query
	switch ep {
	case "summary":
		out, err = summary(ctx, v, q, admin)
	case "quota":
		out, err = quotaResp(ctx, v, admin)
	case "requests":
		out, err = requests(ctx, v, q, admin)
	case "rates":
		out, err = rates(ctx, v, q)
	case "fx":
		out = fxInfo(v)
	default:
		return errorResp(http.StatusNotFound, "not_found", "no such endpoint")
	}
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			return errorResp(ae.status, ae.code, ae.message)
		}
		return errorResp(http.StatusInternalServerError, "internal", "query failed")
	}
	return jsonResp(http.StatusOK, out)
}

func jsonResp(status int, v any) abi.ManagementResponse {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return errorResp(http.StatusInternalServerError, "internal", "encode failed")
	}
	return abi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: bytes.TrimRight(buf.Bytes(), "\n"),
	}
}

func errorResp(status int, code, message string) abi.ManagementResponse {
	type body struct {
		Schema int `json:"schema"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	var b body
	b.Schema = schemaVersion
	b.Error.Code, b.Error.Message = code, message
	r := jsonResp(status, b)
	r.StatusCode = status
	return r
}

// Timestamp formats ms since epoch as RFC 3339 UTC with milliseconds.
func Timestamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func tsPtr(ms int64) *string {
	if ms <= 0 {
		return nil
	}
	return new(Timestamp(ms))
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, badRequest("invalid timestamp " + quoteShort(s) + " (want RFC 3339)")
	}
	return t, nil
}

// parseRange reads since/until (RFC 3339): defaults are the 30 days before
// now, or before until when only until is given; the range must be non-empty
// and at most 5 years.
func parseRange(q url.Values, now time.Time) (since, until time.Time, err error) {
	until, since = now, now.Add(-30*24*time.Hour)
	if s := q.Get("until"); s != "" {
		if until, err = parseTime(s); err != nil {
			return
		}
	}
	if s := q.Get("since"); s != "" {
		if since, err = parseTime(s); err != nil {
			return
		}
	} else if q.Get("until") != "" {
		since = until.Add(-30 * 24 * time.Hour)
	}
	if !since.Before(until) {
		return since, until, badRequest("since must be before until")
	}
	if until.Sub(since) > 5*366*24*time.Hour {
		return since, until, badRequest("range must be at most 5 years")
	}
	return since, until, nil
}

func quoteShort(s string) string {
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	b, _ := json.Marshal(s)
	return string(b)
}
