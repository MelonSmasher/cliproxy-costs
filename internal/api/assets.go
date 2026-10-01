package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/web"
)

type asset struct {
	body        []byte
	contentType string
	etag        string
}

// assetPaths are served under /dashboard/.
var assetPaths = []string{"app.js", "app.css", "chart.js"}

var assets = func() map[string]asset {
	read := func(name string) []byte {
		b, err := web.Files.ReadFile(name)
		if err != nil {
			panic("embedded asset missing: " + name)
		}
		return b
	}
	mk := func(body []byte, ct string) asset {
		sum := sha256.Sum256(body)
		return asset{body: body, contentType: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	}
	css := append(append(read("vendor/uPlot.min.css"), '\n'), read("app.css")...)
	return map[string]asset{
		"":         mk(read("index.html"), "text/html; charset=utf-8"),
		"app.js":   mk(read("app.js"), "text/javascript; charset=utf-8"),
		"app.css":  mk(css, "text/css; charset=utf-8"),
		"chart.js": mk(read("vendor/uPlot.iife.min.js"), "text/javascript; charset=utf-8"),
	}
}()

// frame-ancestors 'self': CPA's management panel embeds plugin resources
// (the "Costs" menu entry) in a same-origin iframe; other origins stay blocked.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"

func serveAsset(name string) abi.ManagementResponse {
	a, ok := assets[name]
	if !ok {
		return errorResp(http.StatusNotFound, "not_found", "no such asset")
	}
	return abi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {a.contentType},
			"Cache-Control":           {"no-cache"},
			"Etag":                    {a.etag},
			"Content-Security-Policy": {csp},
			"X-Content-Type-Options":  {"nosniff"},
			"Referrer-Policy":         {"no-referrer"},
			"X-Frame-Options":         {"SAMEORIGIN"},
		},
		Body: a.body,
	}
}
