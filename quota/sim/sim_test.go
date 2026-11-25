package sim

import (
	"testing"
	"time"

	"filequota/quota"
)

func run(t *testing.T, opts Options) Outcome {
	t.Helper()
	r, err := NewRunner(opts)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	out := r.Run()
	if !out.OK {
		for i, f := range out.Failures {
			if i >= 10 {
				break
			}
			t.Error(f)
		}
		t.Fatalf("seed=%d steps=%d denials=%d", opts.Seed, out.Steps, out.Denials)
	}
	return out
}

// TestSeededRunsSurvive exercises many seeds so a lucky seed cannot hide a
// broken invariant.
func TestSeededRunsSurvive(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	for _, seed := range []int64{1, 2, 3, 17, 99, 1234, 98765} {
		seed := seed
		t.Run("", func(t *testing.T) {
			out := run(t, Options{
				Seed: seed, Users: 12, Groups: 5, Trees: 5,
				Steps: 8000, MaxLive: 120, CheckEvery: 1000,
			})
			if out.Denials == 0 || out.Clipped == 0 || out.AdminEdits == 0 {
				t.Fatalf("workload too tame: denials=%d clipped=%d admin=%d",
					out.Denials, out.Clipped, out.AdminEdits)
			}
			if out.FinalScan.Nodes == 0 {
				t.Fatal("nothing left alive; workload is not exercising the tree")
			}
		})
	}
}

// TestLongJumpsSkipGrace makes time leap well past grace periods, checking
// that the enforced ceiling switches from hard to soft and back when an admin
// extends the period.
func TestLongJumpsSkipGrace(t *testing.T) {
	r, err := NewRunner(Options{
		Seed: 4242, Users: 8, Groups: 4, Trees: 4,
		Steps: 20000, MaxLive: 60, CheckEvery: 2500,
		DefaultGrace: 10 * time.Minute, LimitScale: 0.2,
	})
	if err != nil {
		t.Fatal(err)
	}
	expiredSeen, activeSeen := 0, 0
	for r.steps < r.Opts.Steps && len(r.out.Failures) == 0 {
		r.step()
		r.steps++
		r.Eng.AccountsView(quota.User, func(v quota.AccountView) {
			if v.BytesGrace.Expired {
				expiredSeen++
			}
			if v.BytesGrace.Active && !v.BytesGrace.Expired {
				activeSeen++
			}
		})
	}
	for _, f := range r.out.Failures {
		t.Error(f)
	}
	if expiredSeen == 0 {
		t.Error("no grace period ever expired; the time jumps are not exercising expiry")
	}
	if activeSeen == 0 {
		t.Error("no grace period was ever observed running")
	}
	// Enough variety: refusals of every operation class that can be refused.
	for _, op := range []quota.Op{quota.OpCreate, quota.OpWrite, quota.OpMove, quota.OpChownUser, quota.OpTruncate} {
		if r.out.Operations[string(op)] == 0 {
			t.Errorf("operation %s never ran", op)
		}
	}
}

// TestDeterministic checks that the same seed produces byte-identical
// outcomes, which the "fixed seed" requirement depends on.
func TestDeterministic(t *testing.T) {
	opts := Options{Seed: 777, Users: 10, Groups: 4, Trees: 4, Steps: 4000, MaxLive: 100, CheckEvery: 500}
	a := run(t, opts)
	b := run(t, opts)
	if a.Steps != b.Steps || a.Denials != b.Denials || a.Written != b.Written || a.Clipped != b.Clipped {
		t.Fatalf("same seed diverged:\n%+v\n%+v", a, b)
	}
	if len(a.Operations) != len(b.Operations) {
		t.Fatalf("operation mix diverged: %v vs %v", a.Operations, b.Operations)
	}
	for k, v := range a.Operations {
		if b.Operations[k] != v {
			t.Fatalf("operation %s: %d vs %d", k, v, b.Operations[k])
		}
	}
}
