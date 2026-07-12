package outboundproxy

import "testing"

func TestParseProxyModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		mode Mode
	}{
		{name: "environment", raw: "", mode: Environment},
		{name: "direct", raw: "direct", mode: Direct},
		{name: "http", raw: "http://user:pass@127.0.0.1:8080", mode: Explicit},
		{name: "socks", raw: "socks5h://proxy.example:1080", mode: Explicit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, _, err := Parse(tc.raw)
			if err != nil || mode != tc.mode {
				t.Fatalf("Parse(%q) = mode %d, err %v", tc.raw, mode, err)
			}
		})
	}
}

func TestParseRejectsUnsafeOrIncompleteProxyURLs(t *testing.T) {
	for _, raw := range []string{
		"localhost:8080",
		"http://",
		"http://proxy.example",
		"http://proxy.example:0",
		"http://proxy.example:65536",
		"http://proxy.example:not-a-port",
		"ftp://proxy.example:21",
		"http://proxy.example:8080/path",
		"http://proxy.example:8080?target=elsewhere",
	} {
		if _, _, err := Parse(raw); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}
