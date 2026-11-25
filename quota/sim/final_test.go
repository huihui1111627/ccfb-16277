package sim

import (
	"testing"
)

// TestMillionStepInvariants is the acceptance run: millions of mixed
// operations across dozens of users, groups and trees, with grace periods
// skipped by random clock jumps. It is skipped in -short mode.
func TestMillionStepInvariants(t *testing.T) {
	if testing.Short() {
		t.Skip("long acceptance run")
	}
	out := run(t, Options{
		Seed: 20260101, Users: 48, Groups: 12, Trees: 24,
		Steps: 1_500_000, MaxLive: 600, CheckEvery: 25_000,
	})
	t.Logf("steps=%d denials=%d clipped=%d written=%d admin=%d full_checks=%d elapsed=%.1fs",
		out.Steps, out.Denials, out.Clipped, out.Written, out.AdminEdits, out.FullChecks, out.ElapsedSec)
	for _, op := range []string{"create", "mkdir", "symlink", "write", "truncate", "delete", "move", "hardlink", "chown_user", "chown_group", "set_limits"} {
		if out.Operations[op] == 0 {
			t.Errorf("workload never ran %s", op)
		}
	}
}
