package pricing

import (
	"math"

	"github.com/MelonSmasher/cliproxy-costs/internal/config"
)

// Detail mirrors the usage.handle token counters.
type Detail struct {
	InputTokens, OutputTokens, ReasoningTokens, CachedTokens,
	CacheReadTokens, CacheCreationTokens, TotalTokens int64
}

// NormalizeDetail converts upstream-family counters into non-overlapping
// buckets. mismatch reports that TotalTokens (when > 0) disagrees with the
// family's expected identity. Invalid counters return empty buckets and a mismatch.
func NormalizeDetail(family string, d Detail) (b Buckets, mismatch bool) {
	if !ValidDetail(family, d) {
		return Buckets{}, true
	}
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

// ValidDetail rejects impossible counters before normalization can hide an
// overlap or overflow. A total-token mismatch alone is diagnostic, not invalid.
func ValidDetail(family string, d Detail) bool {
	for _, n := range []int64{d.InputTokens, d.OutputTokens, d.ReasoningTokens, d.CachedTokens, d.CacheReadTokens, d.CacheCreationTokens, d.TotalTokens} {
		if n < 0 {
			return false
		}
	}
	cr := d.CacheReadTokens
	if cr == 0 && (family != config.FamilyAnthropic || d.CacheCreationTokens == 0) {
		cr = d.CachedTokens
	}
	switch family {
	case config.FamilyAnthropic:
		return sumFits(d.InputTokens, cr, d.CacheCreationTokens, d.OutputTokens) && d.ReasoningTokens <= d.OutputTokens
	case config.FamilyGemini:
		return cr <= d.InputTokens && sumFits(d.InputTokens, d.OutputTokens, d.ReasoningTokens)
	default:
		return cr <= d.InputTokens && d.CacheCreationTokens <= d.InputTokens-cr &&
			d.ReasoningTokens <= d.OutputTokens && sumFits(d.InputTokens, d.OutputTokens)
	}
}

func sumFits(values ...int64) bool {
	var total int64
	for _, n := range values {
		if n < 0 || n > math.MaxInt64-total {
			return false
		}
		total += n
	}
	return true
}

// ValidBuckets reports whether non-overlapping buckets can be safely priced.
func ValidBuckets(b Buckets) bool {
	return b.Reasoning >= 0 && b.Reasoning <= b.Output && sumFits(b.Input, b.CacheRead, b.CacheWrite, b.Output)
}
