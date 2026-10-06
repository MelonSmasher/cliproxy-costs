package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

type rateResponse struct {
	Feed struct {
		URL, Status string
		ETag        string `json:"etag"`
		Error       string `json:"error"`
	} `json:"feed"`
	Models []struct {
		Status string             `json:"status"`
		ID     string             `json:"rate_card_id"`
		Rates  map[string]float64 `json:"rates"`
	} `json:"models"`
}

func readRates(t *testing.T, h *nativeHost) rateResponse {
	t.Helper()
	r := h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
	var out rateResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil {
		t.Fatalf("rates query failed: %d %s", r.status, r.body)
	}
	return out
}

func (r rateResponse) hasInputRate(want float64) bool {
	return len(r.Models) == 1 && r.Models[0].Rates["input"] == want
}
