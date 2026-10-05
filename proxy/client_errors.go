package proxy

import "net/http"

// Public failures expose actionable categories, not transport messages, hosts,
// account identifiers or upstream response bodies. Admin traces keep the cause.
func clientErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if upstream, ok := asUpstreamError(err); ok {
		switch upstream.Kind {
		case UpstreamErrorToolAssemblyTimeout, UpstreamErrorToolOutputTruncated:
			return "Tool call did not complete. For file changes, retry with smaller complete edits; preserve any required atomic operation."
		case UpstreamErrorEmptyResponse:
			return "The upstream service returned no usable output. Please retry."
		case UpstreamErrorStreamTruncated:
			return "The response was interrupted before completion."
		}
	}
	switch mapDownstreamError(err).Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "The request was rejected by the upstream service. Check the input and tool history."
	case http.StatusRequestEntityTooLarge:
		return "The request exceeds the supported size."
	case http.StatusForbidden:
		return "The request was denied by the upstream service."
	case http.StatusTooManyRequests:
		return "The service is temporarily busy or its quota is exhausted. Please retry later."
	case http.StatusGatewayTimeout:
		return "The request timed out waiting for the upstream service."
	case http.StatusServiceUnavailable:
		return "The service is temporarily unavailable. Please retry later."
	case 499:
		return "The request was canceled."
	default:
		return "The upstream request failed. Please retry or contact the administrator with the request ID."
	}
}
