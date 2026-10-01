package api

import (
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/config"
	"github.com/MelonSmasher/cliproxy-costs/internal/fx"
)

// ecbPublishUTC approximates the ECB publication time (16:00 CET) on the
// reference date; staleness is measured from it.
const ecbPublishUTC = 15 * time.Hour

type fxResp struct {
	Schema          int                `json:"schema"`
	Base            string             `json:"base"`
	Source          string             `json:"source"`
	Status          string             `json:"status"`
	AsOf            *string            `json:"as_of"`
	FetchedAt       *string            `json:"fetched_at"`
	Error           *string            `json:"error"`
	DisplayCurrency string             `json:"display_currency"`
	Currencies      []string           `json:"currencies"`
	Rates           map[string]float64 `json:"rates"`
}

func fxInfo(v *View) fxResp {
	c := v.Config.Currency
	out := fxResp{Schema: schemaVersion, Base: config.BaseCurrency, Source: c.Source, DisplayCurrency: c.Display, Currencies: c.Currencies}
	switch c.Source {
	case config.FXSourceOff:
		out.Status = "off"
		out.Rates = map[string]float64{config.BaseCurrency: 1}
		return out
	case config.FXSourceFixed:
		out.Status = "ok"
		out.Rates = fx.Rates(c.Currencies, nil, c.Fixed)
		return out
	}
	snap := v.FX.Snapshot
	var perEUR map[string]float64
	if v.FX.Error != "" {
		out.Error = new(v.FX.Error)
	}
	switch {
	case snap == nil:
		out.Status = "error"
		if out.Error == nil {
			out.Error = new("no ECB rates fetched yet")
		}
	default:
		perEUR = snap.PerEUR
		out.AsOf, out.FetchedAt = new(snap.AsOf), tsPtr(snap.FetchedMS)
		day, _ := time.Parse(time.DateOnly, snap.AsOf)
		switch {
		case v.Now().Sub(day.Add(ecbPublishUTC)) > time.Duration(c.StaleAfterHours*float64(time.Hour)):
			out.Status = "stale"
		case out.Error != nil:
			out.Status = "error"
		default:
			out.Status = "ok"
		}
	}
	out.Rates = fx.Rates(c.Currencies, perEUR, c.Fixed)
	return out
}
