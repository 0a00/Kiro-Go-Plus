package config

import "testing"

func TestResolveMaxPayloadBytes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"unset falls back to default", "", DefaultMaxPayloadBytes},
		{"whitespace falls back to default", "   ", DefaultMaxPayloadBytes},
		{"zero disables the byte ceiling", "0", 0},
		{"measured kiro-lb ascii bisect", "1085435", 1085435},
		{"exact minimum is honored", "65536", 65536},
		{"below minimum falls back", "65535", DefaultMaxPayloadBytes},
		{"negative falls back", "-1", DefaultMaxPayloadBytes},
		{"garbage falls back", "many", DefaultMaxPayloadBytes},
		{"padded value is accepted", " 1200000 ", 1200000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMaxPayloadBytes(tc.raw); got != tc.want {
				t.Fatalf("resolveMaxPayloadBytes(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestMaxPayloadBytesIsResolvedOnce(t *testing.T) {
	first := MaxPayloadBytes()
	t.Setenv(MaxPayloadBytesEnv, "999999")
	if second := MaxPayloadBytes(); second != first {
		t.Fatalf("expected the ceiling to stay %d after a later env change, got %d", first, second)
	}
}
