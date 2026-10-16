package fakelux

import "net/http"

// authed is an HTTP client that signs every request with DefaultKey, as
// a luxmux client of New's server does.
var authed = &http.Client{Transport: bearer{DefaultKey}}

type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}
