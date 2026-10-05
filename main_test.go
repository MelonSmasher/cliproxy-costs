package main

import (
	"math"
	"testing"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
)

func TestNativeBufferBounds(t *testing.T) {
	for _, tc := range []struct {
		present bool
		n       uint64
		want    bool
	}{
		{false, 0, true}, {true, 0, true}, {false, 1, false},
		{true, maxRPCBytes, true}, {true, maxRPCBytes + 1, false},
		{true, math.MaxInt32 + 1, false}, {true, math.MaxUint64, false},
	} {
		if got := validBuffer(tc.present, tc.n); got != tc.want {
			t.Errorf("validBuffer(%t, %d) = %t", tc.present, tc.n, got)
		}
	}
}

func TestClosedBridgeDoesNotCallHost(t *testing.T) {
	h := &hostBridge{}
	h.close()
	if _, err := h.Call("host.http.do", nil); err != abi.ErrHostClosed {
		t.Fatalf("closed callback error = %v", err)
	}
	h.close()
}
