// Package sim drives the quota engine with a seeded, reproducible random
// workload and checks quota invariants after every single operation.
//
// The runner keeps its own model of the simulated file system: the directory
// entries, each inode's size, owner, group, tree label and link count, plus
// the configured limits and the grace-period clocks it expects. After every
// operation it compares that model with the engine:
//
//   - per-account byte and inode totals come from the runner's model (built
//     up only from what each operation was supposed to do) and are compared
//     with the engine's counters;
//   - the engine's own VerifyScan recomputes its counters from its inode
//     table, so the two chains together tie the model to the engine;
//   - every refusal is checked against the runner's pre-operation model: the
//     engine must have refused the account and resource that the model says
//     was full, with the ceiling the model predicts, and the request must not
//     have fit;
//   - every successful or partial operation is checked against the predicted
//     outcome, including the number of bytes a clipped write reports.
package sim

import (
	"fmt"
	"math/rand"
	"time"

	"filequota/quota"
)

// Options configures a run.
type Options struct {
	Seed         int64
	Users        int
	Groups       int
	Trees        int
	Steps        int
	MaxLive      int
	Start        time.Time
	DefaultGrace time.Duration
	CheckEvery   int
	MaxWall      time.Duration
	// LimitScale multiplies every configured limit. Tests use a small scale
	// so soft limits are crossed within a few thousand steps.
	LimitScale float64
	Log        func(format string, args ...any)
}

func (o *Options) defaults() {
	if o.Seed == 0 {
		o.Seed = 1
	}
	if o.Users <= 0 {
		o.Users = 40
	}
	if o.Groups <= 0 {
		o.Groups = 12
	}
	if o.Trees <= 0 {
		o.Trees = 10
	}
	if o.Steps <= 0 {
		o.Steps = 300000
	}
	if o.MaxLive <= 0 {
		o.MaxLive = 140
	}
	if o.CheckEvery <= 0 {
		o.CheckEvery = 25000
	}
	if o.Start.IsZero() {
		o.Start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if o.DefaultGrace <= 0 {
		o.DefaultGrace = 45 * time.Minute
	}
	if o.LimitScale <= 0 {
		o.LimitScale = 1
	}
}

// Outcome reports what a run did and found.
type Outcome struct {
	Seed       int64              `json:"seed"`
	Steps      int                `json:"steps"`
	Operations map[string]int     `json:"operations"`
	Denials    int64              `json:"denials"`
	Clipped    int64              `json:"clipped_writes"`
	Written    int64              `json:"bytes_written"`
	FullChecks int                `json:"full_checks"`
	AdminEdits int                `json:"admin_edits"`
	ElapsedSec float64            `json:"elapsed_seconds"`
	Final      quota.VerifyReport `json:"final"`
	FinalScan  quota.VerifyReport `json:"final_scan"`
	Failures   []string           `json:"failures"`
	OK         bool               `json:"ok"`
}

type usageRow struct{ bytes, inodes int64 }

// dims index the per-account arrays.
const (
	dUser = iota
	dGroup
	dTree
)

type limits struct{ bytes, inodes quota.Limit }

type graceClock struct {
	armed    bool
	deadline time.Time
}

type mnode struct {
	ino    quota.NodeID
	kind   quota.NodeKind
	size   int64
	owner  int
	group  int
	tree   int
	links  int
	parent quota.NodeID
	name   string
	kids   map[string]*mnode // directories only
	idx    int               // position in runner.order
}

// Runner is one simulation.
type Runner struct {
	Eng  *quota.Engine
	Opts Options
	rng  *rand.Rand
	out  Outcome

	ids [3][]string
	idx [3]map[string]int

	use  [3][]usageRow
	lim  [3][]limits
	clk  [3][][]graceClock // [dim][account][kind]
	soft [3][][]int64      // cached SoftEffective per account and kind

	nodes map[quota.NodeID]*mnode
	order []*mnode // live nodes, swap-removal
	mount map[quota.NodeID]int

	dirs     []*mnode // cached directories
	treeRoot map[int]quota.NodeID

	nameSeq   int64
	lastOp    quota.Op
	svcUser   string
	svcGroup  string
	debugMove bool
	steps     int
}

// NewRunner bootstraps the file system layout.
func NewRunner(opts Options) (*Runner, error) {
	opts.defaults()
	r := &Runner{
		Opts:     opts,
		rng:      rand.New(rand.NewSource(opts.Seed)),
		nodes:    make(map[quota.NodeID]*mnode),
		mount:    make(map[quota.NodeID]int),
		dirs:     make([]*mnode, 0, 64),
		treeRoot: make(map[int]quota.NodeID),
		out:      Outcome{Seed: opts.Seed, Operations: map[string]int{}},
	}
	for d := range r.idx {
		r.idx[d] = make(map[string]int)
	}

	specs := [3][]quota.AccountSpec{
		accountSpecs("u", opts.Users, opts.DefaultGrace, opts.LimitScale),
		accountSpecs("g", opts.Groups, opts.DefaultGrace, opts.LimitScale),
		accountSpecs("t", opts.Trees, opts.DefaultGrace, opts.LimitScale),
	}
	for d := 0; d < 3; d++ {
		for _, s := range specs[d] {
			r.addAccount(quota.Dimension(d), s)
		}
	}
	treeIDs := append([]string(nil), r.ids[dTree]...)
	// The implicit root tree account is created by the engine; register it so
	// the audit knows about it. Everything created directly under the file
	// system root belongs to it.
	r.addAccount(quota.Tree, quota.AccountSpec{ID: quota.RootTreeID})

	eng, err := quota.New(quota.Config{
		Users: specs[dUser], Groups: specs[dGroup], Trees: specs[dTree],
	}, opts.Start, opts.DefaultGrace)
	if err != nil {
		return nil, err
	}
	r.Eng = eng

	root := &mnode{ino: quota.RootInode, kind: quota.Dir, tree: r.mustIdx(quota.Tree, quota.RootTreeID), kids: map[string]*mnode{}, parent: quota.RootInode}
	r.nodes[quota.RootInode] = root

	// Bootstrap directories with a dedicated, unlimited service account so
	// the skeleton layout cannot trip a workload account's limits. Files are
	// seeded with real accounts, and skipped if a limit refuses them.
	const svcUser, svcGroup = "svc", "svc"
	r.addAccount(quota.User, quota.AccountSpec{ID: svcUser})
	r.addAccount(quota.Group, quota.AccountSpec{ID: svcGroup})
	r.svcUser, r.svcGroup = svcUser, svcGroup
	owner, group := svcUser, svcGroup
	for i, treeID := range treeIDs {
		res, err := eng.Mkdir(owner, group, quota.RootInode, "tree-"+treeID, treeID)
		if err != nil {
			return nil, fmt.Errorf("bootstrap mkdir tree %s: %w", treeID, err)
		}
		ti := r.mustIdx(quota.Tree, treeID)
		tr := r.newNode(res.Ino, quota.Dir, owner, group, ti, 1, quota.RootInode, "tree-"+treeID)
		root.kids[tr.name] = tr
		r.mount[res.Ino] = ti
		r.treeRoot[ti] = res.Ino
		r.chargeUsage(tr, 0, 1)
		_ = i

		work, err := eng.Mkdir(owner, group, res.Ino, "work", "")
		if err != nil {
			// Very small scaled limits can refuse the skeleton itself; that is
			// a configuration problem for the caller, not a workload failure.
			return nil, fmt.Errorf("bootstrap mkdir work (影响目录树 %s 的限额，请调大 LimitScale): %w", treeID, err)
		}
		w := r.newNode(work.Ino, quota.Dir, owner, group, ti, 1, res.Ino, "work")
		tr.kids["work"] = w
		r.chargeUsage(w, 0, 1)

		for k := 0; k < 2; k++ {
			u, g := r.pickUser(), r.pickGroup()
			name := r.newName("seed")
			fr, err := eng.Create(u, g, work.Ino, name)
			if err != nil {
				continue
			}
			f := r.newNode(fr.Ino, quota.File, u, g, ti, 1, work.Ino, name)
			w.kids[name] = f
			r.chargeUsage(f, 0, 1)
			// Seeding is best effort: a small account may refuse part of the
			// sample bytes, which is exactly what the live workload does.
			n := r.rng.Int63n(40 << 10)
			if res, err := eng.Write(fr.Ino, n); res.Written > 0 {
				f.size += res.Written
				r.chargeUsage(f, res.Written, 0)
				_ = err
			}
		}
	}
	if rep := r.Eng.VerifyScan(); !rep.OK {
		return nil, fmt.Errorf("bootstrap verification failed: %v", rep.Problems)
	}
	r.refreshAll()
	return r, nil
}

func accountSpecs(prefix string, n int, grace time.Duration, scale float64) []quota.AccountSpec {
	profiles := []struct{ softB, hardB, softI, hardI int64 }{
		{6 << 20, 14 << 20, 40, 90},
		{4 << 20, 10 << 20, 30, 70},
		{3 << 20, 8 << 20, 24, 60},
		{12 << 20, 32 << 20, 80, 180},
		{2 << 20, 6 << 20, 18, 45},
		{8 << 20, 20 << 20, 50, 120},
		{5 << 20, 12 << 20, 36, 80},
		{40 << 20, 96 << 20, 200, 500},
		{1 << 20, 4 << 20, 12, 30},
	}
	scaleI := func(v int64) int64 {
		if v <= 0 {
			return v
		}
		s := int64(float64(v) * scale)
		if s < 1 {
			s = 1
		}
		return s
	}
	out := make([]quota.AccountSpec, 0, n)
	for i := 0; i < n; i++ {
		p := profiles[i%len(profiles)]
		out = append(out, quota.AccountSpec{
			ID:     fmt.Sprintf("%s-%02d", prefix, i),
			Bytes:  quota.Limit{Soft: scaleI(p.softB), Hard: scaleI(p.hardB)},
			Inodes: quota.Limit{Soft: scaleI(p.softI), Hard: scaleI(p.hardI)},
			Grace:  grace,
		})
	}
	return out
}

func (r *Runner) addAccount(dim quota.Dimension, s quota.AccountSpec) int {
	d := int(dim)
	if i, ok := r.idx[d][s.ID]; ok {
		return i
	}
	i := len(r.ids[d])
	r.ids[d] = append(r.ids[d], s.ID)
	r.idx[d][s.ID] = i
	r.use[d] = append(r.use[d], usageRow{})
	r.lim[d] = append(r.lim[d], limits{bytes: s.Bytes, inodes: s.Inodes})
	r.clk[d] = append(r.clk[d], make([]graceClock, 2))
	r.soft[d] = append(r.soft[d], make([]int64, 2))
	return i
}

func (r *Runner) mustIdx(dim quota.Dimension, id string) int {
	i, ok := r.idx[int(dim)][id]
	if !ok {
		panic("unknown account " + id)
	}
	return i
}

// workloadUsers omits the unlimited bootstrap service account.
func (r *Runner) workloadUsers() []string {
	out := make([]string, 0, len(r.ids[dUser]))
	for _, id := range r.ids[dUser] {
		if id != r.svcUser {
			out = append(out, id)
		}
	}
	return out
}

func (r *Runner) workloadGroups() []string {
	out := make([]string, 0, len(r.ids[dGroup]))
	for _, id := range r.ids[dGroup] {
		if id != r.svcGroup {
			out = append(out, id)
		}
	}
	return out
}

func (r *Runner) pickUser() string {
	u := r.workloadUsers()
	return u[r.rng.Intn(len(u))]
}

func (r *Runner) pickGroup() string {
	g := r.workloadGroups()
	return g[r.rng.Intn(len(g))]
}

func (r *Runner) newNode(ino quota.NodeID, kind quota.NodeKind, owner, group string, tree, links int, parent quota.NodeID, name string) *mnode {
	m := &mnode{
		ino:    ino,
		kind:   kind,
		owner:  r.mustIdx(quota.User, owner),
		group:  r.mustIdx(quota.Group, group),
		tree:   tree,
		links:  links,
		parent: parent,
		name:   name,
		idx:    len(r.order),
	}
	if kind == quota.Dir {
		m.kids = make(map[string]*mnode)
		r.dirs = append(r.dirs, m)
	}
	r.nodes[ino] = m
	r.order = append(r.order, m)
	return m
}

func (r *Runner) dropNode(m *mnode) {
	if m.kind == quota.Dir {
		for i := range r.dirs {
			if r.dirs[i] == m {
				r.dirs = append(r.dirs[:i], r.dirs[i+1:]...)
				break
			}
		}
	}
	delete(r.mount, m.ino)
	last := len(r.order) - 1
	if m.idx != last {
		r.order[m.idx] = r.order[last]
		r.order[m.idx].idx = m.idx
	}
	r.order = r.order[:last]
	delete(r.nodes, m.ino)
}

func (r *Runner) newName(prefix string) string {
	r.nameSeq++
	return fmt.Sprintf("%s-%d", prefix, r.nameSeq)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Opts.Log != nil {
		r.Opts.Log(format, args...)
	}
}

func (r *Runner) fail(format string, args ...any) {
	if len(r.out.Failures) < 20 {
		r.out.Failures = append(r.out.Failures, fmt.Sprintf(format, args...))
	}
}

func (r *Runner) now() time.Time { return r.Eng.Now() }

// ---- model accounting -----------------------------------------------------

func (r *Runner) chargeUsage(m *mnode, bytes, inodes int64) {
	for _, d := range []int{dUser, dGroup, dTree} {
		ai := m.owner
		switch d {
		case dGroup:
			ai = m.group
		case dTree:
			ai = m.tree
		}
		r.use[d][ai].bytes += bytes
		r.use[d][ai].inodes += inodes
	}
}

func (r *Runner) chain() []int { return []int{dUser, dGroup, dTree} }

func (r *Runner) acctIndex(m *mnode, d int) int {
	switch d {
	case dUser:
		return m.owner
	case dGroup:
		return m.group
	}
	return m.tree
}

func (r *Runner) softEff(d, ai int, k quota.Kind) int64 {
	l := r.lim[d][ai]
	var soft, hard int64
	if k == quota.Bytes {
		soft, hard = l.bytes.Soft, l.bytes.Hard
	} else {
		soft, hard = l.inodes.Soft, l.inodes.Hard
	}
	if soft <= 0 || hard <= 0 || soft >= hard {
		return 0
	}
	return soft
}

func (r *Runner) ceiling(d, ai int, k quota.Kind) int64 {
	l := r.lim[d][ai]
	hard := l.bytes.Hard
	used := r.use[d][ai].bytes
	if k == quota.Inodes {
		hard = l.inodes.Hard
		used = r.use[d][ai].inodes
	}
	soft := r.soft[d][ai][k]
	c := r.clk[d][ai][k]
	if soft > 0 && c.armed && !r.now().Before(c.deadline) && used > soft {
		return soft
	}
	return hard
}

func (r *Runner) room(d, ai int, k quota.Kind) (int64, bool) {
	lim := r.ceiling(d, ai, k)
	if lim <= 0 {
		return 0, false
	}
	used := r.use[d][ai].bytes
	if k == quota.Inodes {
		used = r.use[d][ai].inodes
	}
	return lim - used, true
}

// refreshAccount recomputes cached soft limits and grace clocks from usage.
// It is called after every operation, at which point the engine has already
// run its own grace update for the same instant.
func (r *Runner) refreshAccount(d, ai int) {
	for _, k := range []quota.Kind{quota.Bytes, quota.Inodes} {
		soft := r.softEff(d, ai, k)
		r.soft[d][ai][k] = soft
		used := r.use[d][ai].bytes
		if k == quota.Inodes {
			used = r.use[d][ai].inodes
		}
		c := &r.clk[d][ai][k]
		if soft > 0 && used > soft {
			if !c.armed {
				c.armed = true
				c.deadline = r.now().Add(r.Opts.DefaultGrace)
			}
			continue
		}
		c.armed = false
		c.deadline = time.Time{}
	}
}

func (r *Runner) refreshAll() {
	for d := 0; d < 3; d++ {
		for ai := range r.ids[d] {
			r.refreshAccount(d, ai)
		}
	}
}

// ---- the run loop ---------------------------------------------------------

// Run executes the workload.
func (r *Runner) Run() Outcome {
	started := time.Now()
	for r.steps < r.Opts.Steps && len(r.out.Failures) == 0 {
		if r.Opts.MaxWall > 0 && time.Since(started) > r.Opts.MaxWall {
			break
		}
		r.step()
		r.steps++
	}
	r.out.Steps = r.steps
	r.out.Final = r.Eng.VerifyState()
	r.out.FinalScan = r.Eng.VerifyScan()
	for _, p := range r.out.Final.Problems {
		r.fail("最终结构校验: %s", p)
	}
	for _, p := range r.out.FinalScan.Problems {
		r.fail("最终扫描校验: %s", p)
	}
	for _, d := range r.Eng.Denials() {
		if bad := quota.VerifyDenial(d); bad != "" {
			r.fail("拒绝记录: %s", bad)
		}
	}
	r.out.Denials = r.Eng.DenialCount()
	if r.out.Denials == 0 {
		r.fail("整个模拟没有触发任何配额拒绝，负载没有覆盖到限额")
	}
	r.out.ElapsedSec = time.Since(started).Seconds()
	r.out.OK = len(r.out.Failures) == 0
	return r.out
}

func (r *Runner) step() {
	r.advanceClock()
	r.refreshAll()

	live := len(r.order)
	roll := r.rng.Intn(1000)
	switch {
	case roll < 1:
		r.opSetLimits()
	case roll < 2:
		r.opGrantGrace()
	case roll < 3:
		r.advanceClock()
	case live > r.Opts.MaxLive && roll < 260:
		r.opDelete()
	case roll < 200:
		r.opCreateFile()
	case roll < 245:
		r.opMkdir()
	case roll < 290:
		r.opSymlink()
	case roll < 560:
		r.opWrite()
	case roll < 640:
		r.opTruncate()
	case roll < 720:
		r.opDelete()
	case roll < 830:
		r.opMove()
	case roll < 900:
		r.opHardlink()
	case roll < 940:
		r.opCrossTreeLink()
	case roll < 970:
		r.opChownUser()
	default:
		r.opChownGroup()
	}

	r.refreshAll()
	r.audit()
}

func (r *Runner) advanceClock() {
	grace := r.Opts.DefaultGrace
	var d time.Duration
	switch r.rng.Intn(1000) {
	case 0:
		d = time.Duration(1 + r.rng.Int63n(int64(6*grace))) // leap past grace periods
	case 1:
		d = grace + time.Duration(r.rng.Int63n(int64(grace)+1))
	default:
		d = time.Duration(r.rng.Int63n(int64(grace/8) + 1))
	}
	if d > 0 {
		r.Eng.Advance(d)
	}
}
