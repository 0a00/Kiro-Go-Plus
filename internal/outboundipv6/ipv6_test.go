package outboundipv6

import (
	"net/netip"
	"testing"
)

func TestStableAddressStaysInsidePrefix(t *testing.T) {
	value := Config{Mode: ModeStable, Prefix: "2001:db8:1234:5678::/64"}
	first, err := Address(value, "account-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Address(value, "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("stable address changed: %s != %s", first, second)
	}
	prefix := netip.MustParsePrefix(value.Prefix)
	if !prefix.Contains(first) {
		t.Fatalf("address %s escaped %s", first, prefix)
	}
	other, err := Address(value, "account-b")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatalf("different accounts received the same address %s", first)
	}
}

func TestRandomAddressIsStableForProcess(t *testing.T) {
	value := Config{Mode: ModeRandom, Prefix: "2001:db8:abcd::/64"}
	first, err := Address(value, "random-account")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Address(value, "random-account")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("process assignment changed: %s != %s", first, second)
	}
}

func TestNormalizeRejectsUnsafePrefixes(t *testing.T) {
	for _, prefix := range []string{"127.0.0.0/8", "::1/128", "ff00::/8", "2001:db8::/16", "2001:db8::/125"} {
		if _, err := Normalize(Config{Mode: ModeStable, Prefix: prefix}); err == nil {
			t.Fatalf("expected prefix %q to be rejected", prefix)
		}
	}
}

func TestDisabledDoesNotRequirePrefix(t *testing.T) {
	addr, err := Address(Config{Mode: ModeDisabled}, "account")
	if err != nil {
		t.Fatal(err)
	}
	if addr.IsValid() {
		t.Fatalf("disabled mode returned %s", addr)
	}
}
