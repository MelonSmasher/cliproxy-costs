package pricing

import "github.com/MelonSmasher/cliproxy-costs/internal/config"

// Detail mirrors the usage.handle token counters.
type Detail struct {
	InputTokens, OutputTokens, ReasoningTokens, CachedTokens,
	CacheReadTokens, CacheCreationTokens, TotalTokens int64
}

// NormalizeDetail converts upstream-family counters into non-overlapping
// buckets. mismatch reports that TotalTokens (when > 0) disagrees with the
// family's expected identity; it never alters the buckets.
func NormalizeDetail(family string, d Detail) (b Buckets, mismatch bool) {
	var expected int64
	switch family {
	case config.FamilyAnthropic:
		cr := d.CacheReadTokens
		// CPA's CachedTokens falls back to cache creation when read is 0;
		// only trust it as a read count when no creation count exists.
		if cr == 0 && d.CachedTokens > 0 && d.CacheCreationTokens == 0 {
			cr = d.CachedTokens
		}
		b = Buckets{Input: d.InputTokens, CacheRead: cr, CacheWrite: d.CacheCreationTokens, Output: d.OutputTokens, Reasoning: d.ReasoningTokens}
		expected = d.InputTokens + cr + d.CacheCreationTokens + d.OutputTokens
	case config.FamilyGemini:
		cr := d.CacheReadTokens
		if cr == 0 {
			cr = d.CachedTokens
		}
		// Gemini thoughts are separate from candidates and billed as output.
		b = Buckets{Input: nonNeg(d.InputTokens - cr), CacheRead: cr, Output: d.OutputTokens + d.ReasoningTokens, Reasoning: d.ReasoningTokens}
		expected = d.InputTokens + d.OutputTokens + d.ReasoningTokens
	default:
		cr := d.CacheReadTokens
		if cr == 0 {
			cr = d.CachedTokens
		}
		b = Buckets{Input: nonNeg(d.InputTokens - cr - d.CacheCreationTokens), CacheRead: cr, CacheWrite: d.CacheCreationTokens, Output: d.OutputTokens, Reasoning: d.ReasoningTokens}
		expected = d.InputTokens + d.OutputTokens
	}
	return b, d.TotalTokens > 0 && d.TotalTokens != expected
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
