package auth

import "net/http"

// Avoid library defaults and legacy project names without impersonating another
// client. Explicit endpoint-specific headers are preserved.
type authHeaderTransport struct{ base http.RoundTripper }

func (t authHeaderTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t authHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if clone.Header == nil {
		clone.Header = make(http.Header)
	}
	if clone.Header.Get("User-Agent") == "" {
		clone.Header.Set("User-Agent", "HTTPClient/1.0")
	}
	return t.base.RoundTrip(clone)
}
