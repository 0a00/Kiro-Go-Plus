package outboundipv6

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"
)

func TestStableAddressStaysInsidePrefix(t *testing.T) {
	value := Config{Mode: ModeStable, Prefix: "2606:4700:1234:5678::/64"}
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
	value := Config{Mode: ModeRandom, Prefix: "2606:4700:abcd::/64"}
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
	for _, prefix := range []string{"127.0.0.0/8", "::1/128", "ff00::/8", "2001:db8::/32", "fc00::/7", "2606:4700::/16", "2606:4700::/125"} {
		if _, err := Normalize(Config{Mode: ModeStable, Prefix: prefix}); err == nil {
			t.Fatalf("expected prefix %q to be rejected", prefix)
		}
	}
}

func TestRandomAssignmentsDoNotCollide(t *testing.T) {
	value := Config{Mode: ModeRandom, Prefix: "2606:4700:ffff::/124"}
	seen := map[netip.Addr]bool{}
	for i := 0; i < 15; i++ {
		addr, err := Address(value, string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		if seen[addr] {
			t.Fatalf("duplicate random address %s", addr)
		}
		seen[addr] = true
	}
	if _, err := Address(value, "overflow"); err == nil {
		t.Fatal("expected exhausted prefix to fail")
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

func TestValidateAssignmentsRejectsInsufficientCapacity(t *testing.T) {
	ids := make([]string, 16)
	for i := range ids {
		ids[i] = string(rune('a' + i))
	}
	if err := ValidateAssignments(Config{Mode: ModeStable, Prefix: "2606:4700:eeee::/124"}, ids); err == nil {
		t.Fatal("expected insufficient prefix capacity to fail")
	}
}

func TestBindErrorCanBeDetectedThroughWrapping(t *testing.T) {
	err := fmt.Errorf("dial request: %w", WrapBindError("2606:4700::1", errors.New("no route")))
	if !IsBindError(err) {
		t.Fatalf("expected wrapped bind error, got %v", err)
	}
}
