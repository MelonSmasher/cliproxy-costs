package abi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// ErrHTTPBusy means a previous host callback has not returned yet. The host
// owns the transport; do not accumulate abandoned callbacks on repeated timeouts.
var ErrHTTPBusy = errors.New("previous host HTTP operation is still in progress")

// HTTPClient performs HTTP requests exclusively through the host callback API.
// New hosts stream bounded chunks and support operation cancellation. Older
// hosts fall back to a single outstanding callback; they may buffer the entire
// response before its size can be checked, but cancellation stops waiting.
type HTTPClient struct {
	host Host
	slot chan struct{}
	// Feature flags are accessed only by the goroutine holding slot.
	legacyOperation bool
	legacyStream    bool
}

func NewHTTPClient(host Host) *HTTPClient {
	return &HTTPClient{host: host, slot: make(chan struct{}, 1)}
}

// Do returns promptly when ctx is canceled, including if a broken or legacy
// host never returns. Until all callbacks finish, subsequent requests fail
// with ErrHTTPBusy rather than starting more host work.
func (c *HTTPClient) Do(ctx context.Context, req HostHTTPRequest, maxBytes int64) (HostHTTPResponse, error) {
	if maxBytes <= 0 {
		return HostHTTPResponse{}, errors.New("host HTTP response size limit must be positive")
	}
	if err := ctx.Err(); err != nil {
		return HostHTTPResponse{}, err
	}
	select {
	case c.slot <- struct{}{}:
	default:
		return HostHTTPResponse{}, ErrHTTPBusy
	}
	type result struct {
		response HostHTTPResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.perform(ctx, req, maxBytes)
		<-c.slot
		done <- result{resp, err}
	}()
	select {
	case <-ctx.Done():
		return HostHTTPResponse{}, ctx.Err()
	case r := <-done:
		if err := ctx.Err(); err != nil {
			return HostHTTPResponse{}, err
		}
		return r.response, r.err
	}
}

func (c *HTTPClient) perform(ctx context.Context, req HostHTTPRequest, maxBytes int64) (HostHTTPResponse, error) {
	if !c.legacyOperation {
		id, unsupported, err := c.openOperation()
		if err != nil {
			return HostHTTPResponse{}, err
		}
		c.legacyOperation = unsupported
		if !unsupported {
			req.OperationID = id
			// Keep the slot until both Do and Cancel return. A faulty cancel
			// callback must not become another unbounded source of goroutines.
			var once sync.Once
			cancel := func() {
				once.Do(func() {
					_ = CallResult(c.host, MethodHostHTTPCancel, struct {
						OperationID string `json:"operation_id"`
					}{id}, nil)
				})
			}
			stop := context.AfterFunc(ctx, cancel)
			defer func() {
				stop()
				// Cancel is idempotent; this also releases an operation if the
				// request never reached the host (for example after cancellation).
				cancel()
			}()
		}
	}
	if err := ctx.Err(); err != nil {
		return HostHTTPResponse{}, err
	}
	if !c.legacyStream {
		var stream struct {
			StatusCode int         `json:"status_code"`
			Headers    http.Header `json:"headers"`
			StreamID   string      `json:"stream_id"`
		}
		unsupported, err := c.callOptional(MethodHostHTTPDoStream, req, &stream)
		if err != nil {
			return HostHTTPResponse{}, err
		}
		c.legacyStream = unsupported
		if !unsupported {
			if strings.TrimSpace(stream.StreamID) == "" {
				return HostHTTPResponse{}, errors.New("host HTTP stream returned an empty ID")
			}
			streamRequest := struct {
				StreamID string `json:"stream_id"`
			}{stream.StreamID}
			var once sync.Once
			closeStream := func() {
				once.Do(func() { _ = CallResult(c.host, MethodHostHTTPStreamClose, streamRequest, nil) })
			}
			stop := context.AfterFunc(ctx, closeStream)
			defer func() { stop(); closeStream() }()
			resp := HostHTTPResponse{StatusCode: stream.StatusCode, Headers: stream.Headers}
			// Feed consumers need bodies only for HTTP 200. Do not download
			// arbitrary error pages or bodies attached to conditional replies.
			if resp.StatusCode != http.StatusOK {
				return resp, nil
			}
			for {
				if err := ctx.Err(); err != nil {
					return HostHTTPResponse{}, err
				}
				var chunk struct {
					Payload []byte `json:"payload"`
					Error   string `json:"error"`
					Done    bool   `json:"done"`
				}
				if err := CallResult(c.host, MethodHostHTTPStreamRead, streamRequest, &chunk); err != nil {
					return HostHTTPResponse{}, err
				}
				if chunk.Error != "" {
					return HostHTTPResponse{}, fmt.Errorf("host HTTP stream: %s", chunk.Error)
				}
				if int64(len(chunk.Payload)) > maxBytes-int64(len(resp.Body)) {
					return HostHTTPResponse{}, fmt.Errorf("host HTTP response exceeds %d bytes", maxBytes)
				}
				resp.Body = append(resp.Body, chunk.Payload...)
				if chunk.Done {
					return resp, nil
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return HostHTTPResponse{}, err
	}
	var resp HostHTTPResponse
	err := CallResult(c.host, MethodHostHTTPDo, req, &resp)
	if err == nil && int64(len(resp.Body)) > maxBytes {
		return HostHTTPResponse{}, fmt.Errorf("host HTTP response exceeds %d bytes", maxBytes)
	}
	return resp, err
}

func (c *HTTPClient) openOperation() (id string, unsupported bool, err error) {
	var result struct {
		OperationID string `json:"operation_id"`
	}
	unsupported, err = c.callOptional(MethodHostHTTPOperationOpen, struct{}{}, &result)
	if err != nil || unsupported {
		return "", unsupported, err
	}
	if strings.TrimSpace(result.OperationID) == "" {
		return "", false, errors.New("host HTTP operation returned an empty ID")
	}
	return result.OperationID, false, nil
}

// callOptional allows a compatibility fallback only for an explicit missing
// callback. Transport failures, invalid responses and denials fail closed.
func (c *HTTPClient) callOptional(method string, request, out any) (unsupported bool, err error) {
	if c.host == nil {
		return false, ErrHostClosed
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	raw, err := c.host.Call(method, payload)
	if err != nil {
		return false, err
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return false, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			switch env.Error.Code {
			case "unknown_method", "method_not_found", "unsupported_method":
				return true, nil
			case "host_call_failed":
				// CPA's older native bridge uses this generic code. Only its
				// exact unknown-callback message authorizes a legacy fallback.
				if env.Error.Message == "unsupported host callback "+method {
					return true, nil
				}
			}
			return false, fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return false, fmt.Errorf("%s: failed", method)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return false, fmt.Errorf("%s: decode result: %w", method, err)
	}
	return false, nil
}
