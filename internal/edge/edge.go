// Package edge is kpr's front door: a transparent reverse proxy
// in front of the separately-run registry it companions. Slice 1
// forwards bytes and guards Location headers; slice 2 fences
// writes through the same handler. The proxy mints nothing and
// alters no proofs — write proof semantics are unaffected.
package edge

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"nrtn.dev/catalyst/kpr/internal/proof"
)

// ErrNoProof refuses to open the edge without proven relative
// upstream URLs: an absolute backend Location would walk
// clients around the proxy — bypass must not compile.
var ErrNoProof = errors.New("edge refuses without RelativeURLs: prove the registry emits relative Locations first")

// Proxy forwards a registry API. The zero value is useless:
// New validates the backend.
type Proxy struct {
	backend *url.URL

	// Logf receives loud guard lines (rewritten absolute
	// Locations). Nil discards; production passes the startup
	// logger.
	Logf func(format string, args ...any)
}

// New validates the backend URL. Guessing backends is worse
// than not proxying.
func New(backend string) (*Proxy, error) {
	u, err := url.Parse(backend)
	if err != nil {
		return nil, fmt.Errorf("edge: bad backend %q: %w", backend, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("edge: bad backend %q: want scheme://host", backend)
	}
	return &Proxy{backend: u}, nil
}

// Handler returns the transparent handler, gated on the proof:
// nil proof refuses before anything listens. Slice 2 wraps this
// handler with HOLD/DENY fencing; the forwarding stays byte
// identical underneath.
func (p *Proxy) Handler(proven proof.RelativeURLs) (http.Handler, error) {
	if proven == nil {
		return nil, ErrNoProof
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(p.backend)
		},
		ModifyResponse: p.guardLocation,
	}
	return rp, nil
}

// guardLocation keeps the proven case untouched (relative
// Locations pass) and rewrites any absolute upstream Location
// to its path — loudly. Absolute backend addresses are fence
// bypasses; absolute anywhere else is unexpected on a
// filesystem driver and must surface, never silently pass.
func (p *Proxy) guardLocation(resp *http.Response) error {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil
	}
	u, err := url.Parse(loc)
	if err != nil {
		p.logf("edge: unparseable upstream Location %q on %s: leaving", loc, resp.Request.URL.Path)
		return nil
	}
	if !u.IsAbs() {
		return nil
	}
	p.logf("edge: absolute upstream Location %q on %s: rewriting to path", loc, resp.Request.URL.Path)
	resp.Header.Set("Location", u.RequestURI())
	return nil
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}
