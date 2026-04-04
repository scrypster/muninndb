package plugin

import "net/http"

// muninnTransport is an http.RoundTripper that injects MuninnDB identification
// headers on every outbound request to an LLM provider. This lets self-hosters
// see "MuninnDB" in their LLM runner logs instead of the generic Go client name.
type muninnTransport struct {
	base http.RoundTripper
}

// RoundTrip clones the request, sets X-Client-Name and User-Agent, then
// delegates to the base transport.
func (t *muninnTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Client-Name", "MuninnDB")
	if r.Header.Get("User-Agent") == "" {
		r.Header.Set("User-Agent", "MuninnDB")
	}
	return t.base.RoundTrip(r)
}

// WrapTransport wraps base with MuninnDB identification headers. If base is
// nil, http.DefaultTransport is used. All embed and enrich provider HTTP
// clients should pass their transport through this wrapper.
func WrapTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &muninnTransport{base: base}
}
