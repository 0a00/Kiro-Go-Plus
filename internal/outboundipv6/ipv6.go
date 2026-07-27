package outboundipv6

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

type HostInfo struct {
	Container           bool     `json:"container"`
	GlobalAddresses     []string `json:"globalAddresses"`
	CandidatePrefixes   []string `json:"candidatePrefixes"`
	IPNonlocalBind      bool     `json:"ipNonlocalBind"`
	IPNonlocalBindKnown bool     `json:"ipNonlocalBindKnown"`
	RecommendedPrefix   string   `json:"recommendedPrefix,omitempty"`
}

type Stats struct {
	BindFailures uint64 `json:"bindFailures"`
	Fallbacks    uint64 `json:"fallbacks"`
}

type BindError struct {
	Source string
	Err    error
}

func (e *BindError) Error() string {
	if e == nil {
		return "IPv6 source bind failed"
	}
	return fmt.Sprintf("IPv6 source %s failed: %v", e.Source, e.Err)
}

func (e *BindError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

var (
	randomMu          sync.Mutex
	randomAssignments = make(map[string]netip.Addr)
	randomUsed        = make(map[string]map[netip.Addr]string)
	bindFailures      atomic.Uint64
	fallbacks         atomic.Uint64
)

func IsBindError(err error) bool {
	var bindErr *BindError
	return errors.As(err, &bindErr)
}

func WrapBindError(source string, err error) error {
	if err == nil {
		return nil
	}
	bindFailures.Add(1)
	return &BindError{Source: source, Err: err}
}

func RecordFallback() {
	fallbacks.Add(1)
}

func StatsSnapshot() Stats {
	return Stats{BindFailures: bindFailures.Load(), Fallbacks: fallbacks.Load()}
}

func ResetRandomAssignments() {
	randomMu.Lock()
	defer randomMu.Unlock()
	randomAssignments = make(map[string]netip.Addr)
	randomUsed = make(map[string]map[netip.Addr]string)
}

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
	if !isPublicIPv6(prefix.Addr()) {
		return Config{}, fmt.Errorf("prefix must be a public unicast IPv6 network")
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
		return randomAddress(prefix, key, accountID)
	} else {
		digest := sha256.Sum256([]byte(key))
		copy(entropy[:], digest[:])
	}
	addr := merge(prefix, entropy)
	if addr == prefix.Masked().Addr() {
		entropy[15] ^= 1
		addr = merge(prefix, entropy)
	}
	return addr, nil
}

func randomAddress(prefix netip.Prefix, key, accountID string) (netip.Addr, error) {
	randomMu.Lock()
	defer randomMu.Unlock()
	if cached, ok := randomAssignments[key]; ok {
		return cached, nil
	}
	capacity, _ := Capacity(prefix)
	used := randomUsed[prefix.String()]
	if used == nil {
		used = make(map[netip.Addr]string)
		randomUsed[prefix.String()] = used
	}
	if capacity != math.MaxUint64 && uint64(len(used)) >= capacity {
		return netip.Addr{}, fmt.Errorf("IPv6 prefix %s has no unused addresses", prefix)
	}
	for attempt := 0; attempt < 256; attempt++ {
		var entropy [16]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return netip.Addr{}, fmt.Errorf("generate random IPv6: %w", err)
		}
		addr := merge(prefix, entropy)
		if addr == prefix.Masked().Addr() {
			continue
		}
		if _, exists := used[addr]; exists {
			continue
		}
		used[addr] = accountID
		randomAssignments[key] = addr
		return addr, nil
	}
	return netip.Addr{}, fmt.Errorf("could not allocate a unique IPv6 from %s", prefix)
}

func Capacity(prefix netip.Prefix) (uint64, bool) {
	hostBits := 128 - prefix.Bits()
	if hostBits >= 64 {
		return math.MaxUint64, true
	}
	return (uint64(1) << hostBits) - 1, false
}

func ValidateAssignments(value Config, accountIDs []string) error {
	value, err := Normalize(value)
	if err != nil || value.Mode == ModeDisabled {
		return err
	}
	prefix, _ := netip.ParsePrefix(value.Prefix)
	capacity, saturated := Capacity(prefix)
	if !saturated && uint64(len(accountIDs)) > capacity {
		return fmt.Errorf("IPv6 prefix %s provides %d addresses for %d accounts", prefix, capacity, len(accountIDs))
	}
	if value.Mode != ModeStable {
		return nil
	}
	seen := make(map[netip.Addr]string, len(accountIDs))
	for _, accountID := range accountIDs {
		addr, err := Address(value, accountID)
		if err != nil {
			return err
		}
		if previous, ok := seen[addr]; ok && previous != accountID {
			return fmt.Errorf("accounts %q and %q map to the same IPv6 %s; use a larger prefix", previous, accountID, addr)
		}
		seen[addr] = accountID
	}
	return nil
}

func DetectHost() HostInfo {
	info := HostInfo{}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		info.Container = true
	}
	if raw, err := os.ReadFile("/proc/sys/net/ipv6/ip_nonlocal_bind"); err == nil {
		info.IPNonlocalBindKnown = true
		info.IPNonlocalBind = strings.TrimSpace(string(raw)) == "1"
	}
	interfaces, _ := net.Interfaces()
	prefixSet := make(map[string]struct{})
	addressSet := make(map[string]struct{})
	for _, iface := range interfaces {
		addresses, _ := iface.Addrs()
		for _, raw := range addresses {
			prefix, err := netip.ParsePrefix(raw.String())
			if err != nil || !isPublicIPv6(prefix.Addr()) {
				continue
			}
			addressSet[prefix.Addr().String()] = struct{}{}
			prefixSet[prefix.Masked().String()] = struct{}{}
		}
	}
	for value := range addressSet {
		info.GlobalAddresses = append(info.GlobalAddresses, value)
	}
	for value := range prefixSet {
		info.CandidatePrefixes = append(info.CandidatePrefixes, value)
	}
	sort.Strings(info.GlobalAddresses)
	sort.Strings(info.CandidatePrefixes)
	if len(info.CandidatePrefixes) == 1 {
		candidate, _ := netip.ParsePrefix(info.CandidatePrefixes[0])
		if candidate.Bits() <= 96 {
			info.RecommendedPrefix = candidate.String()
		}
	}
	return info
}

func isPublicIPv6(addr netip.Addr) bool {
	if !addr.Is6() || addr.Is4In6() || !addr.IsGlobalUnicast() {
		return false
	}
	for _, blocked := range []string{"fc00::/7", "2001:db8::/32"} {
		prefix := netip.MustParsePrefix(blocked)
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
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
