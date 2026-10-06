package integration

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
)

const requestFailureMode = "CPA_TEST_REQUEST_FAILURE"

// Isolate the expected failure so the regression itself can pass.
func TestRequestCasesStopAfterFailure(t *testing.T) {
	for _, mode := range []string{"chat", "translated"} {
		t.Run(mode, func(t *testing.T) {
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^TestRequestCasesFailureHelper$", "-test.timeout=30s", "-test.v")
			cmd.Env = append((&nativeHost{dir: t.TempDir()}).environment(), requestFailureMode+"="+mode)
			output, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				t.Fatalf("expected failed request subtest: %v\n%s", err, output)
			}
			for _, want := range []string{"CPA trace header missing", "request attempts=1\n"} {
				if !bytes.Contains(output, []byte(want)) {
					t.Errorf("missing failure evidence %q:\n%s", want, output)
				}
			}
			if bytes.Contains(output, []byte("reached later accounting")) {
				t.Fatalf("request failure did not stop parent test:\n%s", output)
			}
		})
	}
}

func TestRequestCasesFailureHelper(t *testing.T) {
	mode := os.Getenv(requestFailureMode)
	if mode == "" {
		t.Skip("only run in the request failure subprocess")
	}
	h := missingTraceHost(t)
	switch mode {
	case "chat":
		runChatCases(t, h)
	case "translated":
		runTranslatedCases(t, h)
	default:
		t.Fatalf("unknown request failure mode %q", mode)
	}
	t.Log("reached later accounting")
}

// The response passes cost checks, then fails before its expectation is added.
func missingTraceHost(t *testing.T) *nativeHost {
	t.Helper()
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("X-CliProxy-Pricing", "override")
		w.Header().Set("X-CliProxy-Cost-USD", "0.002640")
		_, _ = w.Write([]byte(`{"content":"Synthetic fixture OK","usage":{"cost":0.00264,"cost_details":{"pricing_status":"override","rate_card_id":"synthetic"}}}`))
	}))
	t.Cleanup(func() {
		server.Close()
		t.Logf("request attempts=%d", attempts.Load())
	})
	return &nativeHost{t: t, base: server.URL, client: server.Client()}
}
