package pool

import (
	"fmt"
	"kiro-go/config"
	"testing"
	"time"
)

func BenchmarkAccountSelectionBalanced100(b *testing.B) {
	accounts := make([]config.Account, 100)
	modelLists := make(map[string]map[string]bool, len(accounts))
	for index := range accounts {
		id := fmt.Sprintf("account-%03d", index)
		accounts[index] = config.Account{ID: id, Enabled: true, AccessToken: "token", Weight: 1, Priority: index % 3}
		modelLists[id] = map[string]bool{"claude-sonnet-5": index%2 == 0}
	}
	pool := &AccountPool{
		accounts: accounts, cooldowns: make(map[string]time.Time), modelNegative: make(map[modelAvailabilityKey]time.Time),
		modelLists: modelLists, refreshFailures: make(map[string]time.Time), weightedCurrent: make(map[string]int64),
	}
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		selected := pool.selectionIndexesLocked("claude-sonnet-5", nil, now, true, "balanced")
		if len(selected) == 0 {
			b.Fatal("no account selected")
		}
		pool.commitWeightedSelectionLocked(accounts[selected[0]].ID, selected, "balanced")
	}
}
