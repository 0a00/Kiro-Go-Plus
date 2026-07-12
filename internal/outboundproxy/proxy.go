package outboundproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Mode uint8

const (
	Environment Mode = iota
	Direct
	Explicit
)

// Parse validates an outbound proxy setting without exposing credentials in
// returned errors. Empty means use the process proxy environment; direct
// explicitly disables proxy use.
func Parse(raw string) (Mode, *url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Environment, nil, nil
	}
	if strings.EqualFold(raw, "direct") {
		return Direct, nil, nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return 0, nil, fmt.Errorf("invalid proxy URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return 0, nil, fmt.Errorf("proxy scheme must be http, https, socks5, or socks5h")
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return 0, nil, fmt.Errorf("proxy host is required")
	}
	port := parsed.Port()
	if port == "" {
		return 0, nil, fmt.Errorf("proxy port is required")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return 0, nil, fmt.Errorf("proxy port must be between 1 and 65535")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return 0, nil, fmt.Errorf("proxy URL must not contain a path")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, nil, fmt.Errorf("proxy URL must not contain a query or fragment")
	}
	parsed.Scheme = scheme
	return Explicit, parsed, nil
}

func Validate(raw string) error {
	_, _, err := Parse(raw)
	return err
}

// Apply configures proxy behavior on an HTTP transport.
func Apply(transport *http.Transport, raw string) error {
	mode, parsed, err := Parse(raw)
	if err != nil {
		return err
	}
	switch mode {
	case Environment:
		transport.Proxy = http.ProxyFromEnvironment
	case Direct:
		transport.Proxy = nil
	case Explicit:
		transport.Proxy = http.ProxyURL(parsed)
		transport.ForceAttemptHTTP2 = false
	}
	return nil
}
