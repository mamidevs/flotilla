// Package dispatcher exposes SOCKS5 + HTTP CONNECT front-ends that delegate
// outbound dials to the pool. Pin/tag hints can be supplied via SOCKS5 auth
// user, HTTP header, or query parameter — see tag.go.
package dispatcher

import (
	"net"
	"net/http"
	"strings"

	"github.com/mamidevs/flotilla/internal/pool"
)

// HintHeader is the canonical header for selecting a worker over HTTP.
//   X-Flotilla-Node: tr-1                      → pin to worker tr-1
//   X-Flotilla-Tags: region:tr,scraper:a       → tag-filtered selection
//   X-Flotilla-Client: scraper-instance-3      → sticky key override
const (
	HintHeaderNode   = "X-Flotilla-Node"
	HintHeaderTags   = "X-Flotilla-Tags"
	HintHeaderClient = "X-Flotilla-Client"
)

// HintFromHTTP extracts a pool.Hint from an HTTP request's headers.
// Recognized header values are stripped before the request is forwarded.
func HintFromHTTP(r *http.Request) pool.Hint {
	h := pool.Hint{}
	if v := strings.TrimSpace(r.Header.Get(HintHeaderNode)); v != "" {
		h.PinName = v
	}
	if v := strings.TrimSpace(r.Header.Get(HintHeaderTags)); v != "" {
		h.Tags = splitCSV(v)
	}
	if v := strings.TrimSpace(r.Header.Get(HintHeaderClient)); v != "" {
		h.ClientID = v
	} else if addr, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		h.ClientID = addr
	}

	// Strip Flotilla-private headers before tunneling upstream.
	r.Header.Del(HintHeaderNode)
	r.Header.Del(HintHeaderTags)
	r.Header.Del(HintHeaderClient)
	return h
}

// HintFromSocksUser maps an armon/go-socks5 authenticated user to a Hint.
// Supported user-string syntax (any combo, separated by ';'):
//
//	"node=tr-1"                    → pin
//	"tags=region:tr,scraper:a"     → tag filter
//	"client=scraper-3"             → sticky key
//	"tr-1"                         → if it matches a known worker, treat as pin
func HintFromSocksUser(user string, knownWorker func(name string) bool) pool.Hint {
	h := pool.Hint{}
	user = strings.TrimSpace(user)
	if user == "" {
		return h
	}
	if !strings.ContainsAny(user, "=;,") && knownWorker(user) {
		h.PinName = user
		return h
	}
	for _, part := range strings.Split(user, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "node":
			h.PinName = v
		case "tags":
			h.Tags = splitCSV(v)
		case "client":
			h.ClientID = v
		}
	}
	return h
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}
