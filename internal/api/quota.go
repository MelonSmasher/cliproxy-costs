package api

import (
	"context"
	"encoding/json"

	"github.com/MelonSmasher/cliproxy-costs/internal/quota"
)

type quotaWindow struct {
	ID           string  `json:"id"`
	Label        string  `json:"label"`
	DurationMS   int64   `json:"duration_ms"`
	UsedPercent  float64 `json:"used_percent"`
	UsedFraction float64 `json:"used_fraction"`
	ResetsAt     *string `json:"resets_at"`
	Status       string  `json:"status"`
}

type quotaCredential struct {
	Credential string         `json:"credential"`
	AuthID     *string        `json:"auth_id,omitempty"`
	Provider   string         `json:"provider"`
	Label      string         `json:"label"`
	ObservedAt string         `json:"observed_at"`
	Stale      bool           `json:"stale"`
	Plan       string         `json:"plan,omitempty"`
	Windows    []quotaWindow  `json:"windows"`
	Credits    *quota.Credits `json:"credits,omitempty"`
}

// CredentialLabel returns the configured label or "<provider> <cred[:6]>".
func (v *View) CredentialLabel(credential, provider string) string {
	if l := v.Config.CredentialLabels[credential]; l != "" {
		return l
	}
	for _, s := range v.Config.Subscriptions {
		if s.Credential == credential && s.Label != "" {
			return s.Label
		}
	}
	short := credential
	if len(short) > 6 {
		short = short[:6]
	}
	return provider + " " + short
}

func quotaResp(ctx context.Context, v *View) (any, error) {
	obs, err := v.Store.Quotas(ctx)
	if err != nil {
		return nil, err
	}
	now := v.Now()
	staleMS := int64(v.Config.Quota.StaleAfterMinutes * 60000)
	creds := make([]quotaCredential, 0, len(obs))
	for _, o := range obs {
		var s quota.Snapshot
		if err := json.Unmarshal(o.SnapshotJSON, &s); err != nil {
			continue
		}
		stale := now.UnixMilli()-o.ObservedAtMS > staleMS
		c := quotaCredential{
			Credential: o.Credential,
			Provider:   o.Provider,
			Label:      v.CredentialLabel(o.Credential, o.Provider),
			ObservedAt: Timestamp(o.ObservedAtMS),
			Stale:      stale,
			Plan:       s.Plan,
			Windows:    make([]quotaWindow, 0, len(s.Windows)),
			Credits:    s.Credits,
		}
		if o.AuthID != "" {
			c.AuthID = new(o.AuthID)
		}
		for _, w := range s.Windows {
			st := w.Status
			if stale {
				st = quota.StatusUnknown
			}
			c.Windows = append(c.Windows, quotaWindow{
				ID: w.ID, Label: w.Label, DurationMS: w.DurationMS,
				UsedPercent: w.UsedPercent, UsedFraction: w.UsedFraction,
				ResetsAt: tsPtr(w.ResetsAtMS), Status: st,
			})
		}
		creds = append(creds, c)
	}
	return struct {
		Schema       int               `json:"schema"`
		GeneratedAt  string            `json:"generated_at"`
		StaleAfterMS int64             `json:"stale_after_ms"`
		Credentials  []quotaCredential `json:"credentials"`
	}{schemaVersion, Timestamp(now.UnixMilli()), staleMS, creds}, nil
}
