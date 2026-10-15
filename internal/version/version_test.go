package version

import (
	"strings"
	"testing"
)

func TestStringIncludesInjectedValues(t *testing.T) {
	Version, Commit, Date = "v1.2.3", "abc1234", "2026-01-01"
	t.Cleanup(func() { Version, Commit, Date = "dev", "none", "unknown" })

	got := String()
	for _, want := range []string{"v1.2.3", "abc1234", "2026-01-01"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}
