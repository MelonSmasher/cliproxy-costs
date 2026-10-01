package quota

import (
	"net/http"
	"testing"
	"time"
)

var observed = time.Unix(1790000000, 0)

func parse(t *testing.T, h http.Header) (Snapshot, bool) {
	t.Helper()
	s, _, ok := Parse(Filter(h), observed)
	return s, ok
}

func win(s Snapshot, id string) *Window {
	for i := range s.Windows {
		if s.Windows[i].ID == id {
			return &s.Windows[i]
		}
	}
	return nil
}

func TestCodexPrimarySecondaryCreditsAndResetForms(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "42")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
	h.Set("X-Codex-Secondary-Used-Percent", "85")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Reset-At", "1790086400")
	h.Set("X-Codex-Credits-Has-Credits", "true")
	h.Set("X-Codex-Credits-Balance", "12.50")
	h.Set("X-Codex-Plan-Type", "pro")
	h.Set("Authorization", "Bearer must-not-be-kept")
	s, ok := parse(t, h)
	if !ok || len(s.Windows) != 2 || s.Plan != "pro" {
		t.Fatalf("%+v", s)
	}
	p := win(s, "codex:primary:300")
	if p.Label != "5 Hour" || p.DurationMS != 18000000 || p.UsedFraction != 0.42 || p.Status != StatusOK || p.ResetsAtMS != observed.UnixMilli()+3600000 {
		t.Fatalf("primary %+v", p)
	}
	sec := win(s, "codex:secondary:10080")
	if sec.Label != "7 Day" || sec.Status != StatusWarning || sec.ResetsAtMS != 1790086400000 {
		t.Fatalf("secondary %+v", sec)
	}
	if s.Credits == nil || !s.Credits.HasCredits || s.Credits.Balance != "12.50" {
		t.Fatalf("credits %+v", s.Credits)
	}
	if _, kept := Filter(h)["authorization"]; kept {
		t.Fatal("non-quota header kept")
	}
}

func TestCodexAdditionalLimitsAndExhaustion(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Bengalfox-Primary-Used-Percent", "100")
	h.Set("X-Codex-Bengalfox-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Bengalfox-Primary-Reset-After-Seconds", "60")
	h.Set("X-Codex-Additional-Spark-Secondary-Used-Percent", "10")
	h.Set("X-Codex-Additional-Spark-Secondary-Window-Minutes", "1440")
	h.Set("X-Codex-Additional-Spark-Secondary-Reset-At", "1790050000")
	// Same duration as the additional window: must stay a distinct window.
	h.Set("X-Codex-Primary-Used-Percent", "5")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "60")
	s, ok := parse(t, h)
	if !ok || len(s.Windows) != 3 {
		t.Fatalf("%+v", s.Windows)
	}
	if w := win(s, "codex:bengalfox-primary:300"); w == nil || w.Status != StatusExhausted {
		t.Fatalf("bengalfox %+v", w)
	}
	if w := win(s, "codex:spark-secondary:1440"); w == nil || w.Label != "1 Day" {
		t.Fatalf("spark %+v", w)
	}
}

func TestCodexInvalidRangesRejected(t *testing.T) {
	for name, h := range map[string]map[string]string{
		"over 100":      {"Used-Percent": "101", "Window-Minutes": "300", "Reset-After-Seconds": "1"},
		"zero minutes":  {"Used-Percent": "1", "Window-Minutes": "0", "Reset-After-Seconds": "1"},
		"no reset":      {"Used-Percent": "1", "Window-Minutes": "300"},
		"negative used": {"Used-Percent": "-1", "Window-Minutes": "300", "Reset-After-Seconds": "1"},
	} {
		hh := http.Header{}
		for k, v := range h {
			hh.Set("X-Codex-Primary-"+k, v)
		}
		if _, ok := parse(t, hh); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestClaudeUtilizationFractionToPercent(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.53")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1790010000")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.2")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	s, ok := parse(t, h)
	if !ok || len(s.Windows) != 2 {
		t.Fatalf("%+v", s)
	}
	five := win(s, "claude:5h:300")
	if five.UsedPercent != 53 || five.UsedFraction != 0.53 || five.DurationMS != 18000000 || five.ResetsAtMS != 1790010000000 {
		t.Fatalf("5h %+v", five)
	}
	if seven := win(s, "claude:7d:10080"); seven.Status != StatusExhausted || seven.Label != "7 Day" {
		t.Fatalf("7d %+v", seven)
	}
}

// Model-scoped weekly windows arrive as 7d_<model> (seen in the wild as
// 7d_sonnet); they used to be dropped. Anthropic reports Fable only through
// its usage endpoint, not these headers, so "fable" here is just a fixture.
func TestClaudeModelScopedWindows(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.39")
	h.Set("Anthropic-Ratelimit-Unified-7d_fable-Utilization", "0.81")
	h.Set("Anthropic-Ratelimit-Unified-7d_fable-Reset", "1790010000")
	h.Set("Anthropic-Ratelimit-Unified-7d_fable-Status", "allowed_warning")
	h.Set("Anthropic-Ratelimit-Unified-7d_claude_design-Utilization", "0.1")
	h.Set("Anthropic-Ratelimit-Unified-7d_Bad!-Utilization", "0.5")
	s, ok := parse(t, h)
	if !ok || len(s.Windows) != 3 {
		t.Fatalf("%+v", s)
	}
	fable := win(s, "claude:7d_fable:10080")
	if fable.Label != "Fable 7 Day" || fable.UsedPercent != 81 || fable.DurationMS != 604800000 || fable.ResetsAtMS != 1790010000000 || fable.Status != StatusWarning {
		t.Fatalf("fable %+v", fable)
	}
	if d := win(s, "claude:7d_claude_design:10080"); d.Label != "Claude Design 7 Day" {
		t.Fatalf("design %+v", d)
	}
	if all := win(s, "claude:7d:10080"); all.Label != "7 Day" {
		t.Fatalf("all-model 7d %+v", all)
	}
}

func TestHeaderlessRecordYieldsNoSnapshot(t *testing.T) {
	h := http.Header{"Content-Type": {"application/json"}, "X-Codex-Plan-Type": {"pro"}}
	if _, ok := parse(t, h); ok {
		t.Fatal("record without windows must not replace a snapshot")
	}
}
