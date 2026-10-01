package abi

import (
	"encoding/json"
	"errors"
	"fmt"
)

// OK encodes a success envelope around result (nil → `{}`).
func OK(result any) []byte {
	raw := json.RawMessage(`{}`)
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return Fail("internal", "encode result: "+err.Error())
		}
		raw = b
	}
	out, _ := json.Marshal(Envelope{OK: true, Result: raw})
	return out
}

// Fail encodes an error envelope.
func Fail(code, message string) []byte {
	out, _ := json.Marshal(Envelope{OK: false, Error: &Error{Code: code, Message: message}})
	return out
}

// Host is the subset of host callbacks the plugin uses. The cgo bridge in the
// main package implements it; tests use fakes.
type Host interface {
	Call(method string, request []byte) ([]byte, error)
}

// ErrHostClosed is returned for host calls after shutdown began.
var ErrHostClosed = errors.New("host callbacks closed")

// CallResult invokes a host method and decodes the envelope result into out.
func CallResult(h Host, method string, request any, out any) error {
	if h == nil {
		return ErrHostClosed
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	raw, err := h.Call(method, payload)
	if err != nil {
		return err
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("%s: failed", method)
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}
