package usagebody

import (
	"testing"

	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/tidwall/gjson"
)

func TestMessagesStreamRequiresObservedInputCounter(t *testing.T) {
	cases := []struct {
		name, start, delta string
		priced             bool
		cost               float64
	}{
		{"missing start", `{}`, `{"output_tokens":20}`, false, 0},
		{"start without input", `{"output_tokens":0}`, `{"output_tokens":20}`, false, 0},
		{"cache counter alone", `{"cache_read_input_tokens":5}`, `{"output_tokens":20}`, false, 0},
		{"invalid start", `{"input_tokens":"10"}`, `{"output_tokens":20}`, false, 0},
		{"explicit zero start", `{"input_tokens":0}`, `{"output_tokens":20}`, true, 0.0002},
		{"explicit zero delta", `{}`, `{"input_tokens":0,"output_tokens":20}`, true, 0.0002},
		{"self contained delta", `{}`, `{"input_tokens":10,"output_tokens":20}`, true, 0.00022},
		{"normal merge", `{"input_tokens":10,"output_tokens":0}`, `{"output_tokens":20}`, true, 0.00022},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := ReadMessagesUsage(gjson.Parse(tc.start))
			called := false
			pricer := func(b pricing.Buckets) (Annotation, bool) { called = true; return price(b) }
			res := Stream(FormatMessages, []byte(`{"type":"message_delta","usage":`+tc.delta+`}`), &start, pricer)
			if called != tc.priced || (res.Body != nil) != tc.priced || res.Done != tc.priced {
				t.Fatalf("priced incomplete or skipped complete usage: %+v called=%v", res, called)
			}
			if tc.priced && gjson.GetBytes(res.Body, "usage.cost").Float() != tc.cost {
				t.Fatalf("cost=%s want=%g", res.Body, tc.cost)
			}
		})
	}
}
