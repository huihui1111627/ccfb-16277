package quota

import (
	"errors"
	"testing"
	"time"
)

func newTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e, err := New(cfg, start, time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func mustOK(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", what, err)
	}
}

func mustQuota(t *testing.T, what string, err error) *QuotaError {
	t.Helper()
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("%s: want quota error, got %T %v", what, err, err)
	}
	if bad := VerifyDenial(qe.Denial); bad != "" {
		t.Fatalf("%s: %s", what, bad)
	}
	return qe
}

func mustOp(t *testing.T, what string, err error) {
	t.Helper()
	var oe *OpError
	if !errors.As(err, &oe) {
		t.Fatalf("%s: want structural error, got %T %v", what, err, err)
	}
}

func treeFixture(t *testing.T) (*Engine, NodeID, NodeID) {
	t.Helper()
	e := newTestEngine(t, Config{
		Users:  []AccountSpec{{ID: "alice", Bytes: Limit{Soft: 1000, Hard: 2000}, Inodes: Limit{Soft: 10, Hard: 20}}},
		Groups: []AccountSpec{{ID: "staff", Bytes: Limit{Soft: 1000, Hard: 2000}, Inodes: Limit{Soft: 10, Hard: 20}}},
		Trees: []AccountSpec{
			{ID: "town-a", Bytes: Limit{Soft: 3000, Hard: 5000}, Inodes: Limit{Soft: 30, Hard: 60}},
			{ID: "town-b", Bytes: Limit{Soft: 3000, Hard: 5000}, Inodes: Limit{Soft: 30, Hard: 60}},
		},
	})
	a, err := e.Mkdir("alice", "staff", RootInode, "a", "town-a")
	mustOK(t, "mkdir a", err)
	b, err := e.Mkdir("alice", "staff", RootInode, "b", "town-b")
	mustOK(t, "mkdir b", err)
	return e, a.Ino, b.Ino
}

func TestHardLimitRefusesCreate(t *testing.T) {
	e := newTestEngine(t, Config{
		Users: []AccountSpec{{ID: "u", Inodes: Limit{Hard: 2}}},
	})
	for i := 0; i < 2; i++ {
		_, err := e.Create("u", "g", RootInode, string(rune('a'+i)))
		mustOK(t, "create within limit", err)
	}
	_, err := e.Create("u", "g", RootInode, "c")
	qe := mustQuota(t, "third create", err)
	if qe.Denial.Dim != User || qe.Denial.Kind != Inodes || qe.Denial.Limit != 2 {
		t.Fatalf("unexpected denial: %+v", qe.Denial)
	}
}

func TestSoftLimitAllowsWriteThenGraceExpires(t *testing.T) {
	e := newTestEngine(t, Config{
		Users: []AccountSpec{{ID: "u", Bytes: Limit{Soft: 100, Hard: 1000}}},
	})
	if _, err := e.Write(createFile(t, e, "u", "g", "f"), 200); err != nil {
		t.Fatalf("write above soft: %v", err)
	}
	v, _ := e.AccountView(User, "u")
	if !v.BytesGrace.Active || v.BytesGrace.Expired {
		t.Fatalf("grace should be running: %+v", v.BytesGrace)
	}
	if v.BytesLimitNow != 1000 {
		t.Fatalf("hard limit should still apply during grace: %d", v.BytesLimitNow)
	}
	e.Advance(time.Hour + time.Second)
	v, _ = e.AccountView(User, "u")
	if !v.BytesGrace.Expired {
		t.Fatalf("grace should be expired: %+v", v.BytesGrace)
	}
	if v.BytesLimitNow != 100 {
		t.Fatalf("after grace, soft limit must be enforced: %d", v.BytesLimitNow)
	}
	// Already above the enforced soft limit: any write is refused, even one
	// byte.
	_, err := e.Write(1, 1)
	qe := mustQuota(t, "write after grace", err)
	if !qe.Denial.GraceExpired {
		t.Fatalf("denial should be flagged as grace expiry: %+v", qe.Denial)
	}
}

func TestGraceGrantRestoresHardLimit(t *testing.T) {
	e := newTestEngine(t, Config{
		Users: []AccountSpec{{ID: "u", Bytes: Limit{Soft: 100, Hard: 1000}}},
	})
	ino := createFile(t, e, "u", "g", "f")
	_, err := e.Write(ino, 200)
	mustOK(t, "write", err)
	e.Advance(2 * time.Hour)
	if _, err := e.Write(ino, 1); err == nil {
		t.Fatal("expected refusal after grace expiry")
	}
	mustOK(t, "grant grace", e.GrantGrace(User, "u", 3*time.Hour, "admin"))
	e.Advance(time.Hour)
	v, _ := e.AccountView(User, "u")
	if v.BytesLimitNow != 1000 || v.BytesGrace.Expired {
		t.Fatalf("grant should reinstate hard limit: %+v limit=%d", v.BytesGrace, v.BytesLimitNow)
	}
	if _, err := e.Write(ino, 50); err != nil {
		t.Fatalf("write during extension should succeed: %v", err)
	}
	acts := e.ActivityLog()
	if len(acts) != 1 || acts[0].Action != OpGrantGrace || acts[0].Actor != "admin" {
		t.Fatalf("audit log missing grant: %+v", acts)
	}
}

func TestPartialWriteReportsActualBytes(t *testing.T) {
	e := newTestEngine(t, Config{
		Users: []AccountSpec{{ID: "u", Bytes: Limit{Hard: 1000}}},
	})
	ino := createFile(t, e, "u", "g", "f")
	mustOK(t, "first write", ig(e.Write(ino, 700)))
	res, err := e.Write(ino, 500)
	qe := mustQuota(t, "clipped write", err)
	if res.Written != 300 {
		t.Fatalf("want 300 bytes written, got %d", res.Written)
	}
	if qe.Denial.Written != 300 || qe.Denial.Requested != 500 {
		t.Fatalf("denial should report 300/500: %+v", qe.Denial)
	}
	info, _ := e.NodeInfo(ino)
	if info.Size != 1000 {
		t.Fatalf("file should be exactly at the limit, got %d", info.Size)
	}
	if v, _ := e.AccountView(User, "u"); v.BytesUsed != 1000 {
		t.Fatalf("usage should be 1000, got %d", v.BytesUsed)
	}
	// Zero-room write: nothing written, refusal with 0 bytes.
	res, err = e.Write(ino, 10)
	mustQuota(t, "zero room write", err)
	if res.Written != 0 {
		t.Fatalf("want 0 bytes written, got %d", res.Written)
	}
}

func TestTruncateFreesAndClips(t *testing.T) {
	e := newTestEngine(t, Config{
		Users: []AccountSpec{{ID: "u", Bytes: Limit{Hard: 1000}}},
	})
	ino := createFile(t, e, "u", "g", "f")
	mustOK(t, "write", ig(e.Write(ino, 800)))
	res, err := e.Truncate(ino, 100)
	mustOK(t, "shrink", err)
	if res.Freed != 700 {
		t.Fatalf("want 700 freed, got %d", res.Freed)
	}
	if v, _ := e.AccountView(User, "u"); v.BytesUsed != 100 {
		t.Fatalf("usage should drop immediately: %d", v.BytesUsed)
	}
	res, err = e.Truncate(ino, 5000)
	mustQuota(t, "grow past limit", err)
	if res.Size != 1000 || res.Written != 900 {
		t.Fatalf("grow should clip to 1000 (900 added): %+v", res)
	}
}

// ig ignores the result value of a call that returns (Result, error).
func ig[T any](_ T, err error) error { return err }

func createFile(t *testing.T, e *Engine, owner, group, name string) NodeID {
	t.Helper()
	res, err := e.Create(owner, group, RootInode, name)
	mustOK(t, "create "+name, err)
	return res.Ino
}

func TestThreeDimensionsAllRefuse(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		prep func(t *testing.T, e *Engine, ino NodeID)
		want Dimension
	}{
		{
			name: "user",
			cfg: Config{
				Users:  []AccountSpec{{ID: "u", Bytes: Limit{Hard: 100}}},
				Groups: []AccountSpec{{ID: "g", Bytes: Limit{Hard: 10000}}},
				Trees:  []AccountSpec{{ID: "t", Bytes: Limit{Hard: 10000}}},
			},
			want: User,
		},
		{
			name: "group",
			cfg: Config{
				Users:  []AccountSpec{{ID: "u", Bytes: Limit{Hard: 10000}}},
				Groups: []AccountSpec{{ID: "g", Bytes: Limit{Hard: 100}}},
				Trees:  []AccountSpec{{ID: "t", Bytes: Limit{Hard: 10000}}},
			},
			want: Group,
		},
		{
			name: "tree",
			cfg: Config{
				Users:  []AccountSpec{{ID: "u", Bytes: Limit{Hard: 10000}}},
				Groups: []AccountSpec{{ID: "g", Bytes: Limit{Hard: 10000}}},
				Trees:  []AccountSpec{{ID: "t", Bytes: Limit{Hard: 100}}},
			},
			want: Tree,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, tc.cfg)
			dir, err := e.Mkdir("u", "g", RootInode, "d", "t")
			mustOK(t, "mkdir", err)
			res, err := e.Create("u", "g", dir.Ino, "f")
			mustOK(t, "create", err)
			_, err = e.Write(res.Ino, 500)
			qe := mustQuota(t, "write", err)
			if qe.Denial.Dim != tc.want {
				t.Fatalf("want %s to refuse, got %s (%+v)", tc.want, qe.Denial.Dim, qe.Denial)
			}
			if qe.Denial.Written != 100 {
				t.Fatalf("should write 100 bytes, got %d", qe.Denial.Written)
			}
		})
	}
}

func TestTreeInheritanceAndMoveTransfer(t *testing.T) {
	e, dirA, dirB := treeFixture(t)
	sub, err := e.Mkdir("alice", "staff", dirA, "sub", "")
	mustOK(t, "mkdir sub", err)
	res, err := e.Create("alice", "staff", sub.Ino, "f")
	mustOK(t, "create", err)
	info, _ := e.NodeInfo(res.Ino)
	if info.Tree != "town-a" {
		t.Fatalf("nested file should inherit town-a, got %q", info.Tree)
	}
	mustOK(t, "write", ig(e.Write(res.Ino, 1000)))

	beforeA, _ := e.AccountView(Tree, "town-a")
	beforeB, _ := e.AccountView(Tree, "town-b")
	if _, err := e.Move(dirA, "sub", dirB, "sub"); err != nil {
		t.Fatalf("move: %v", err)
	}
	afterA, _ := e.AccountView(Tree, "town-a")
	afterB, _ := e.AccountView(Tree, "town-b")
	if afterA.BytesUsed != beforeA.BytesUsed-1000 || afterA.InodesUsed != beforeA.InodesUsed-2 {
		t.Fatalf("town-a should lose the subtree: before %+v after %+v", beforeA, afterA)
	}
	if afterB.BytesUsed != beforeB.BytesUsed+1000 || afterB.InodesUsed != beforeB.InodesUsed+2 {
		t.Fatalf("town-b should gain the subtree: before %+v after %+v", beforeB, afterB)
	}
	info, _ = e.NodeInfo(res.Ino)
	if info.Tree != "town-b" {
		t.Fatalf("moved file should now be town-b, got %q", info.Tree)
	}
	if rep := e.VerifyState(); !rep.OK {
		t.Fatalf("verify: %v", rep.Problems)
	}
}

func TestMoveRefusedWhenDestinationTreeFull(t *testing.T) {
	e := newTestEngine(t, Config{
		Trees: []AccountSpec{
			{ID: "src", Bytes: Limit{Hard: 10000}, Inodes: Limit{Hard: 100}},
			{ID: "dst", Bytes: Limit{Hard: 500}, Inodes: Limit{Hard: 100}},
		},
	})
	a, err := e.Mkdir("u", "g", RootInode, "a", "src")
	mustOK(t, "mkdir a", err)
	b, err := e.Mkdir("u", "g", RootInode, "b", "dst")
	mustOK(t, "mkdir b", err)
	res, err := e.Create("u", "g", a.Ino, "f")
	mustOK(t, "create", err)
	mustOK(t, "write", ig(e.Write(res.Ino, 900)))

	before, _ := e.AccountView(Tree, "dst")
	_, err = e.Move(a.Ino, "f", b.Ino, "f")
	qe := mustQuota(t, "move", err)
	if qe.Denial.Dim != Tree || qe.Denial.Kind != Bytes {
		t.Fatalf("unexpected denial: %+v", qe.Denial)
	}
	after, _ := e.AccountView(Tree, "dst")
	if before != after {
		t.Fatalf("refused move must not change anything: %+v -> %+v", before, after)
	}
	// The file is still reachable in the source tree.
	if _, ok := e.NodeInfo(res.Ino); !ok {
		t.Fatal("file vanished after refused move")
	}
	if rep := e.VerifyState(); !rep.OK {
		t.Fatalf("verify: %v", rep.Problems)
	}
}

func TestChownTransfersAndRefuses(t *testing.T) {
	e := newTestEngine(t, Config{
		Users: []AccountSpec{
			{ID: "u1", Bytes: Limit{Hard: 10000}, Inodes: Limit{Hard: 100}},
			{ID: "u2", Bytes: Limit{Hard: 300}, Inodes: Limit{Hard: 100}},
			{ID: "u3", Bytes: Limit{Hard: 10000}, Inodes: Limit{Hard: 100}},
		},
	})
	ino := createFile(t, e, "u1", "g", "f")
	mustOK(t, "write", ig(e.Write(ino, 400)))
	res, err := e.ChownOwner(ino, "u3")
	mustOK(t, "chown", err)
	_ = res
	u1, _ := e.AccountView(User, "u1")
	u3, _ := e.AccountView(User, "u3")
	if u1.BytesUsed != 0 || u1.InodesUsed != 0 {
		t.Fatalf("old owner should be empty: %+v", u1)
	}
	if u3.BytesUsed != 400 || u3.InodesUsed != 1 {
		t.Fatalf("new owner should hold the file: %+v", u3)
	}
	// Moving to u2 (300 byte hard limit) must be refused.
	before, _ := e.AccountView(User, "u2")
	_, err = e.ChownOwner(ino, "u2")
	mustQuota(t, "chown to full user", err)
	after, _ := e.AccountView(User, "u2")
	if before != after {
		t.Fatalf("refused chown must not modify the target: %+v -> %+v", before, after)
	}
	u3, _ = e.AccountView(User, "u3")
	if u3.BytesUsed != 400 {
		t.Fatalf("refused chown must not release the source: %+v", u3)
	}
}

func TestDirectoriesAndSymlinksCountAsInodes(t *testing.T) {
	e := newTestEngine(t, Config{Users: []AccountSpec{{ID: "u"}}})
	if _, err := e.Mkdir("u", "g", RootInode, "d", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Symlink("u", "g", RootInode, "l", "d"); err != nil {
		t.Fatal(err)
	}
	v, _ := e.AccountView(User, "u")
	if v.InodesUsed != 2 {
		t.Fatalf("directory and symlink must each count one inode, got %d", v.InodesUsed)
	}
	if v.BytesUsed != 0 {
		t.Fatalf("neither should consume bytes, got %d", v.BytesUsed)
	}
}

func TestDeleteReleasesOnlyWithLastLink(t *testing.T) {
	e := newTestEngine(t, Config{Users: []AccountSpec{{ID: "u"}}})
	ino := createFile(t, e, "u", "g", "f")
	mustOK(t, "write", ig(e.Write(ino, 500)))
	mustOK(t, "link", ig(e.Link(RootInode, "h", ino)))
	if _, err := e.Delete(RootInode, "f"); err != nil {
		t.Fatal(err)
	}
	v, _ := e.AccountView(User, "u")
	if v.BytesUsed != 500 || v.InodesUsed != 1 {
		t.Fatalf("one link left, usage must remain: %+v", v)
	}
	if _, err := e.Delete(RootInode, "h"); err != nil {
		t.Fatal(err)
	}
	v, _ = e.AccountView(User, "u")
	if v.BytesUsed != 0 || v.InodesUsed != 0 {
		t.Fatalf("last link removed, usage must be released: %+v", v)
	}
}

func TestEmptyDirectoryDeleteOnly(t *testing.T) {
	e := newTestEngine(t, Config{})
	d, err := e.Mkdir("u", "g", RootInode, "d", "")
	mustOK(t, "mkdir", err)
	createFile(t, e, "u", "g2", "x")
	_, err = e.Create("u", "g", d.Ino, "child")
	mustOK(t, "create child", err)
	_, err = e.Delete(RootInode, "d")
	mustOp(t, "delete non-empty dir", err)
	if _, err := e.Delete(d.Ino, "child"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Delete(RootInode, "d"); err != nil {
		t.Fatalf("delete empty dir: %v", err)
	}
}

func TestHardlinkCrossTreeIsStructural(t *testing.T) {
	e, dirA, dirB := treeFixture(t)
	res, err := e.Create("alice", "staff", dirA, "f")
	mustOK(t, "create", err)
	mustOK(t, "same-tree link", ig(e.Link(dirA, "h", res.Ino)))
	_, err = e.Link(dirB, "x", res.Ino)
	mustOp(t, "cross-tree link", err)
}

func TestSetLimitsBelowUsageRefused(t *testing.T) {
	e := newTestEngine(t, Config{Users: []AccountSpec{{ID: "u"}}})
	ino := createFile(t, e, "u", "g", "f")
	mustOK(t, "write", ig(e.Write(ino, 300)))
	err := e.SetLimits(User, "u", [2]Limit{{Hard: 100}, {}}, "admin")
	if err == nil {
		t.Fatal("setting a hard limit below usage must fail")
	}
	mustOK(t, "raise", e.SetLimits(User, "u", [2]Limit{{Hard: 1000}, {}}, "admin"))
	acts := e.ActivityLog()
	if len(acts) != 1 || acts[0].Action != OpSetLimits || acts[0].Actor != "admin" {
		t.Fatalf("audit log: %+v", acts)
	}
	if len(acts[0].Changes) == 0 {
		t.Fatal("audit entry must describe the change")
	}
}

func TestSoftAboveHardIsIgnored(t *testing.T) {
	// New rejects soft > hard, so build a valid account and edit the limit
	// directly: the engine must treat such a soft limit as absent rather than
	// arming a grace period it can never honour.
	e2 := newTestEngine(t, Config{Users: []AccountSpec{{ID: "u", Bytes: Limit{Hard: 500}}}})
	a := e2.acct(User, e2.ensureIndex(User, "u"))
	a.Limits[Bytes] = Limit{Soft: 1000, Hard: 500}
	ino := createFile(t, e2, "u", "g", "f")
	res, err := e2.Write(ino, 400)
	mustOK(t, "write", err)
	_ = res
	v, _ := e2.AccountView(User, "u")
	if v.BytesGrace.Active {
		t.Fatalf("soft above hard must not arm grace: %+v", v.BytesGrace)
	}
}

func TestSoftLimitWithoutHardLimit(t *testing.T) {
	e := newTestEngine(t, Config{Users: []AccountSpec{{ID: "u", Bytes: Limit{Soft: 100}}}})
	ino := createFile(t, e, "u", "g", "f")
	if _, err := e.Write(ino, 150); err != nil {
		t.Fatalf("soft limit alone should not block: %v", err)
	}
	v, _ := e.AccountView(User, "u")
	if !v.BytesGrace.Active || v.BytesGrace.Expired {
		t.Fatalf("grace should be running: %+v", v.BytesGrace)
	}
	if v.BytesLimitNow != 0 {
		t.Fatalf("no hard limit means no ceiling during grace, got %d", v.BytesLimitNow)
	}
	e.Advance(time.Hour + time.Second)
	v, _ = e.AccountView(User, "u")
	if !v.BytesGrace.Expired || v.BytesLimitNow != 100 {
		t.Fatalf("expired grace must enforce the soft limit: limit=%d grace=%+v", v.BytesLimitNow, v.BytesGrace)
	}
	_, err := e.Write(ino, 1)
	qe := mustQuota(t, "write after soft-only grace expiry", err)
	if qe.Denial.Limit != 100 || !qe.Denial.GraceExpired {
		t.Fatalf("unexpected denial: %+v", qe.Denial)
	}
	// Shrinking below the soft limit clears the grace state entirely.
	if _, err := e.Truncate(ino, 50); err != nil {
		t.Fatal(err)
	}
	v, _ = e.AccountView(User, "u")
	if v.BytesGrace.Active || v.BytesLimitNow != 0 {
		t.Fatalf("usage back under soft should clear grace: %+v limit=%d", v.BytesGrace, v.BytesLimitNow)
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := New(Config{Users: []AccountSpec{{ID: "u", Bytes: Limit{Soft: 10, Hard: 5}}}}, start, time.Hour); err == nil {
		t.Fatal("soft > hard must be rejected")
	}
	if _, err := New(Config{Users: []AccountSpec{{ID: ""}}}, start, time.Hour); err == nil {
		t.Fatal("empty id must be rejected")
	}
	if _, err := New(Config{Users: []AccountSpec{{ID: "u", Bytes: Limit{Hard: -1}}}}, start, time.Hour); err == nil {
		t.Fatal("negative limit must be rejected")
	}
}
