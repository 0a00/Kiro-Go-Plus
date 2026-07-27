package outboundipv6

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"strings"
	"sync"
)

const (
	ModeDisabled = "disabled"
	ModeRandom   = "random"
	ModeStable   = "stable"
)

type Config struct {
	Mode            string `json:"mode"`
	Prefix          string `json:"prefix"`
	FallbackEnabled bool   `json:"fallbackEnabled"`
}

var randomAssignments sync.Map

func Normalize(value Config) (Config, error) {
	value.Mode = strings.ToLower(strings.TrimSpace(value.Mode))
	value.Prefix = strings.TrimSpace(value.Prefix)
	if value.Mode == "" {
		value.Mode = ModeDisabled
	}
	switch value.Mode {
	case ModeDisabled:
		return value, nil
	case ModeRandom, ModeStable:
	default:
		return Config{}, fmt.Errorf("mode must be disabled, random, or stable")
	}
	prefix, err := netip.ParsePrefix(value.Prefix)
	if err != nil || !prefix.Addr().Is6() {
		return Config{}, fmt.Errorf("prefix must be a valid IPv6 CIDR")
	}
	if !prefix.Addr().IsGlobalUnicast() {
		return Config{}, fmt.Errorf("prefix must be a unicast IPv6 network")
	}
	if prefix.Bits() < 32 || prefix.Bits() > 124 {
		return Config{}, fmt.Errorf("prefix length must be between /32 and /124")
	}
	value.Prefix = prefix.Masked().String()
	return value, nil
}

func Address(value Config, accountID string) (netip.Addr, error) {
	value, err := Normalize(value)
	if err != nil {
		return netip.Addr{}, err
	}
	if value.Mode == ModeDisabled {
		return netip.Addr{}, nil
	}
	prefix, _ := netip.ParsePrefix(value.Prefix)
	key := value.Prefix + "\x00" + accountID
	var entropy [16]byte
	if value.Mode == ModeRandom {
		if cached, ok := randomAssignments.Load(key); ok {
			return cached.(netip.Addr), nil
		}
		if _, err := rand.Read(entropy[:]); err != nil {
			return netip.Addr{}, fmt.Errorf("generate random IPv6: %w", err)
		}
	} else {
		digest := sha256.Sum256([]byte(key))
		copy(entropy[:], digest[:])
	}
	addr := merge(prefix, entropy)
	if value.Mode == ModeRandom {
		actual, _ := randomAssignments.LoadOrStore(key, addr)
		addr = actual.(netip.Addr)
	}
	return addr, nil
}

func merge(prefix netip.Prefix, entropy [16]byte) netip.Addr {
	base := prefix.Masked().Addr().As16()
	bits := prefix.Bits()
	for i := 0; i < len(base); i++ {
		byteBits := bits - i*8
		switch {
		case byteBits >= 8:
		case byteBits <= 0:
			base[i] = entropy[i]
		default:
			mask := byte(0xff << (8 - byteBits))
			base[i] = (base[i] & mask) | (entropy[i] &^ mask)
		}
	}
	return netip.AddrFrom16(base)
}
