// Package quota turns allowlisted upstream response headers into per-
// credential subscription quota snapshots. Pure; no I/O.
package quota

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Window statuses.
const (
	StatusOK        = "ok"
	StatusWarning   = "warning"
	StatusExhausted = "exhausted"
	StatusUnknown   = "unknown"
)

// Window is one quota window.
type Window struct {
	ID           string  `json:"id"`
	Label        string  `json:"label"`
	DurationMS   int64   `json:"duration_ms"`
	UsedPercent  float64 `json:"used_percent"`
	UsedFraction float64 `json:"used_fraction"`
	ResetsAtMS   int64   `json:"resets_at_ms"`
	Status       string  `json:"status"`
}

// Credits is the Codex credit balance.
type Credits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

// Snapshot is the latest parsed quota state of one credential.
type Snapshot struct {
	Plan         string   `json:"plan,omitempty"`
	Windows      []Window `json:"windows"`
	Credits      *Credits `json:"credits,omitempty"`
	RetryAfterMS int64    `json:"retry_after_ms,omitempty"`
}

// Allowed reports whether a header name is kept from a usage record: Codex
// and Anthropic rate-limit families plus Retry-After. Everything else is
// dropped before any storage.
func Allowed(name string) bool {
	l := strings.ToLower(name)
	return l == "retry-after" || strings.HasPrefix(l, "x-codex-") || strings.HasPrefix(l, "anthropic-ratelimit-unified-")
}

// Filter returns the allowlisted headers (last value, bounded length) keyed
// by lowercase name.
func Filter(h http.Header) map[string]string {
	var out map[string]string
	for k, vs := range h {
		if len(vs) == 0 || !Allowed(k) {
			continue
		}
		v := strings.TrimSpace(vs[len(vs)-1])
		if v == "" || len(v) > 512 {
			continue
		}
		if out == nil {
			out = make(map[string]string, 8)
		}
		out[strings.ToLower(k)] = v
	}
	return out
}

// WindowLabel renders a window duration in minutes.
func WindowLabel(minutes int64) string {
	switch {
	case minutes == 300:
		return "5 Hour"
	case minutes == 1440:
		return "1 Day"
	case minutes == 10080:
		return "7 Day"
	case minutes%1440 == 0:
		return strconv.FormatInt(minutes/1440, 10) + " Day"
	case minutes%60 == 0:
		return strconv.FormatInt(minutes/60, 10) + " Hour"
	}
	return strconv.FormatInt(minutes, 10) + " Min"
}

func statusFor(pct float64) string {
	switch {
	case pct >= 100:
		return StatusExhausted
	case pct >= 80:
		return StatusWarning
	}
	return StatusOK
}

// codexReserved are X-Codex-<name>- segments that are not limit namespaces.
var codexReserved = map[string]bool{
	"credits": true, "allowed": true, "limit": true, "active": true, "plan": true, "code": true,
	"primary": true, "secondary": true, "additional": true,
}

// Parse builds a snapshot from filtered headers. ok=false when no window was
// parsed (a header-less record must leave the previous snapshot untouched).
func Parse(h map[string]string, observed time.Time) (Snapshot, string, bool) {
	var s Snapshot
	family := ""
	if w := parseCodex(h, observed, &s); w {
		family = "codex"
	}
	if parseClaude(h, observed, &s) && family == "" {
		family = "claude"
	}
	if len(s.Windows) == 0 {
		return Snapshot{}, "", false
	}
	if v, ok := h["retry-after"]; ok {
		if sec, err := strconv.ParseFloat(v, 64); err == nil && sec >= 0 {
			s.RetryAfterMS = int64(sec * 1000)
		} else if t, err := http.ParseTime(v); err == nil {
			s.RetryAfterMS = max(0, t.Sub(observed).Milliseconds())
		}
	}
	sort.Slice(s.Windows, func(i, j int) bool {
		if s.Windows[i].DurationMS != s.Windows[j].DurationMS {
			return s.Windows[i].DurationMS < s.Windows[j].DurationMS
		}
		return s.Windows[i].ID < s.Windows[j].ID
	})
	return s, family, true
}

func parseCodex(h map[string]string, observed time.Time, s *Snapshot) bool {
	limitReached := strings.EqualFold(h["x-codex-limit-reached"], "true")
	found := false
	add := func(prefix, ns string, reached bool) {
		used, err1 := strconv.ParseFloat(h[prefix+"used-percent"], 64)
		minutes, err2 := strconv.ParseInt(h[prefix+"window-minutes"], 10, 64)
		if err1 != nil || err2 != nil || used < 0 || used > 100 || math.IsNaN(used) || minutes <= 0 {
			return
		}
		var reset int64
		if at, err := strconv.ParseInt(h[prefix+"reset-at"], 10, 64); err == nil && at > 0 {
			reset = at * 1000
		} else if after, err := strconv.ParseFloat(h[prefix+"reset-after-seconds"], 64); err == nil && after >= 0 {
			reset = observed.UnixMilli() + int64(after*1000)
		} else {
			return
		}
		st := statusFor(used)
		if reached && used >= 100 {
			st = StatusExhausted
		}
		s.Windows = append(s.Windows, Window{
			ID:           "codex:" + ns + ":" + strconv.FormatInt(minutes, 10),
			Label:        WindowLabel(minutes),
			DurationMS:   minutes * 60000,
			UsedPercent:  used,
			UsedFraction: used / 100,
			ResetsAtMS:   reset,
			Status:       st,
		})
		found = true
	}
	for _, w := range []string{"primary", "secondary"} {
		add("x-codex-"+w+"-", w, limitReached)
	}
	// Additional limits: X-Codex-Additional-<n>-{Primary,Secondary}-… (websocket
	// origin) and X-Codex-<n>-{Primary,Secondary}-… (HTTP origin).
	seen := map[string]bool{}
	for k := range h {
		rest, ok := strings.CutPrefix(k, "x-codex-")
		if !ok {
			continue
		}
		prefix := "x-codex-"
		if r, ok := strings.CutPrefix(rest, "additional-"); ok {
			rest, prefix = r, prefix+"additional-"
		}
		for _, w := range []string{"-primary-", "-secondary-"} {
			i := strings.Index(rest, w)
			if i <= 0 {
				continue
			}
			name := rest[:i]
			if prefix == "x-codex-" && codexReserved[name] || strings.HasPrefix(name, "code-review") {
				continue
			}
			key := prefix + name + w
			if seen[key] {
				continue
			}
			seen[key] = true
			add(key, name+"-"+strings.Trim(w, "-"), strings.EqualFold(h[prefix+name+"-limit-reached"], "true"))
		}
	}
	if !found {
		return false
	}
	if p := h["x-codex-plan-type"]; p != "" {
		s.Plan = p
	}
	has, hasOK := h["x-codex-credits-has-credits"]
	unl, unlOK := h["x-codex-credits-unlimited"]
	bal, balOK := h["x-codex-credits-balance"]
	if hasOK || unlOK || balOK {
		s.Credits = &Credits{HasCredits: strings.EqualFold(has, "true"), Unlimited: strings.EqualFold(unl, "true"), Balance: bal}
	}
	return true
}

// claudeMinutes parses a unified window name: 5h, 7d, <n>h, <n>d, <n>m. A
// model-scoped window (7d_sonnet, 7d_fable, …) is split off by claudeWindow.
func claudeMinutes(w string) (int64, bool) {
	if len(w) < 2 {
		return 0, false
	}
	n, err := strconv.ParseInt(w[:len(w)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	switch w[len(w)-1] {
	case 'm':
		return n, true
	case 'h':
		return n * 60, true
	case 'd':
		return n * 1440, true
	}
	return 0, false
}

// claudeWindow splits a unified window name into its duration and optional
// model scope: "7d" → (10080, ""), "7d_fable" → (10080, "fable").
func claudeWindow(w string) (int64, string, bool) {
	base, scope, _ := strings.Cut(w, "_")
	minutes, ok := claudeMinutes(base)
	if !ok {
		return 0, "", false
	}
	for _, r := range scope {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return 0, "", false
		}
	}
	return minutes, scope, true
}

// scopeLabel renders a model scope for display: "fable" → "Fable",
// "claude_design" → "Claude Design".
func scopeLabel(scope string) string {
	parts := strings.Split(scope, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func parseClaude(h map[string]string, _ time.Time, s *Snapshot) bool {
	const p = "anthropic-ratelimit-unified-"
	found := false
	for k, v := range h {
		rest, ok := strings.CutPrefix(k, p)
		if !ok {
			continue
		}
		w, ok := strings.CutSuffix(rest, "-utilization")
		if !ok {
			continue
		}
		minutes, scope, ok := claudeWindow(w)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		pct := math.Round(f*1000) / 10
		st := statusFor(pct)
		switch strings.ToLower(h[p+w+"-status"]) {
		case "rejected":
			st = StatusExhausted
		case "allowed_warning":
			if st == StatusOK {
				st = StatusWarning
			}
		}
		var reset int64
		if at, err := strconv.ParseInt(h[p+w+"-reset"], 10, 64); err == nil && at > 0 {
			reset = at * 1000
		}
		label := WindowLabel(minutes)
		if scope != "" {
			// "7 Day" + "fable" → "Fable 7 Day": the model first, so it reads
			// as a separate limit, not a variant of the shared one.
			label = scopeLabel(scope) + " " + label
		}
		s.Windows = append(s.Windows, Window{
			ID:           "claude:" + w + ":" + strconv.FormatInt(minutes, 10),
			Label:        label,
			DurationMS:   minutes * 60000,
			UsedPercent:  pct,
			UsedFraction: math.Round(f*1e6) / 1e6,
			ResetsAtMS:   reset,
			Status:       st,
		})
		found = true
	}
	return found
}
