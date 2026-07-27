package proxy

import (
	"errors"
	"kiro-go/internal/outboundipv6"
	"net/http"
	"net/netip"
	"testing"
)

func TestIPv6BindFailureIsLocalConfigurationError(t *testing.T) {
	err := classifyTransportError("Kiro Runtime", outboundipv6.WrapBindError("2606:4700::1", errors.New("cannot assign requested address")))
	if err.Kind != UpstreamErrorLocalConfiguration {
		t.Fatalf("kind = %q, want %q", err.Kind, UpstreamErrorLocalConfiguration)
	}
	if shouldRetryAcrossAccounts(err) || shouldRetryAcrossEndpoints(err) {
		t.Fatalf("local configuration failure must not retry: %+v", err)
	}
	if got := mapDownstreamError(err).Status; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestLooksLikeDNS64Address(t *testing.T) {
	ipv4 := []netip.Addr{netip.MustParseAddr("192.0.2.1")}
	if !looksLikeDNS64Address(netip.MustParseAddr("64:ff9b::c000:201"), ipv4) {
		t.Fatal("expected synthesized DNS64 address to be detected")
	}
	if looksLikeDNS64Address(netip.MustParseAddr("2606:4700:4700::1111"), ipv4) {
		t.Fatal("native IPv6 was incorrectly classified as DNS64")
	}
}

func TestIPv6BindFailureDuringRefreshDoesNotRotateAccounts(t *testing.T) {
	err := classifyRefreshFailure("token_refresh", outboundipv6.WrapBindError("2606:4700::2", errors.New("no route")))
	if err.Kind != UpstreamErrorLocalConfiguration || shouldRetryAcrossAccounts(err) {
		t.Fatalf("refresh bind failure was not isolated: %+v", err)
	}
}
