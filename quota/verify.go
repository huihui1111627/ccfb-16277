package quota

import (
	"fmt"
	"sort"
)

// VerifyScan independently recomputes every account's usage by iterating all
// live inodes and summing each inode's charge once, using the labels stored on
// the inode. It never reads Account.Used while accumulating, then compares the
// two and checks that no account exceeds a hard limit and no usage is
// negative. It does not check tree labels against inheritance; use
// VerifyState for that.
//
// It is O(live inodes + accounts) and reuses scratch buffers, so the
// randomized simulation can run it after every single operation.
func (e *Engine) VerifyScan() VerifyReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	e.verifyMu.Lock()
	defer e.verifyMu.Unlock()
	rep := VerifyReport{OK: true, At: e.now}
	e.resetUsageScratch()
	for _, n := range e.live {
		e.addUse(User, n.owner, Inodes, 1)
		e.addUse(Group, n.group, Inodes, 1)
		e.addUse(Tree, n.tree, Inodes, 1)
		if n.size != 0 {
			e.addUse(User, n.owner, Bytes, n.size)
			e.addUse(Group, n.group, Bytes, n.size)
			e.addUse(Tree, n.tree, Bytes, n.size)
		}
	}
	e.compareUsage(&rep)
	rep.Nodes = int64(len(e.live))
	for _, n := range e.live {
		rep.Bytes += n.size
	}
	return rep
}

// VerifyState does everything VerifyScan does, and additionally re-derives
// tree membership by walking the directory hierarchy from the root, switching
// at explicit tree roots, and checks the structural invariants: every inode is
// reachable, its recorded links match the directory entries pointing at it,
// and its stored tree label matches inheritance.
func (e *Engine) VerifyState() VerifyReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	e.verifyMu.Lock()
	defer e.verifyMu.Unlock()

	rep := VerifyReport{OK: true, At: e.now}
	e.resetUsageScratch()

	s := &e.scratch
	s.gen++
	if s.gen == 0 {
		for i := range s.treeStamp {
			s.treeStamp[i] = 0
		}
		for i := range s.entryStamp {
			s.entryStamp[i] = 0
		}
		for i := range s.seenStamp {
			s.seenStamp[i] = 0
		}
		s.gen = 1
	}
	gen := s.gen
	need := int(e.nextIno) + 1
	if cap(s.treeStamp) < need {
		s.treeStamp = make([]uint32, need)
		s.entryStamp = make([]uint32, need)
		s.entryCount = make([]int32, need)
		s.seenStamp = make([]uint32, need)
	} else {
		s.treeStamp = s.treeStamp[:need]
		s.entryStamp = s.entryStamp[:need]
		s.entryCount = s.entryCount[:need]
		s.seenStamp = s.seenStamp[:need]
	}
	s.treeLabel = growInt32(s.treeLabel, need)
	setTree := func(ino NodeID, label int) {
		s.treeStamp[ino] = gen
		s.treeLabel[ino] = int32(label)
	}
	getTree := func(ino NodeID) (int, bool) {
		if int(ino) >= len(s.treeStamp) || s.treeStamp[ino] != gen {
			return 0, false
		}
		return int(s.treeLabel[ino]), true
	}
	bumpEntry := func(ino NodeID) {
		if s.entryStamp[ino] != gen {
			s.entryStamp[ino] = gen
			s.entryCount[ino] = 0
		}
		s.entryCount[ino]++
	}

	root := e.nodes[RootInode]
	if root == nil {
		rep.Problems = append(rep.Problems, "根目录不存在")
		rep.OK = false
		return rep
	}
	setTree(RootInode, root.tree)
	s.stack = append(s.stack[:0], RootInode)
	var unique int64

	for len(s.stack) > 0 {
		ino := s.stack[len(s.stack)-1]
		s.stack = s.stack[:len(s.stack)-1]
		dir := e.nodes[ino]
		if dir == nil || dir.kind != Dir {
			rep.Problems = append(rep.Problems, fmt.Sprintf("目录 %d 不存在或不是目录", ino))
			continue
		}
		label, _ := getTree(ino)
		for name, childIno := range dir.dir {
			child := e.nodes[childIno]
			if child == nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf(
					"目录 %d 的目录项 %q 指向不存在的 inode %d", ino, name, childIno))
				continue
			}
			bumpEntry(childIno)
			childLabel := label
			if child.kind == Dir {
				if m, ok := e.mounts[childIno]; ok {
					childLabel = m
				}
				setTree(childIno, childLabel)
				s.stack = append(s.stack, childIno)
			}
			if s.seenStamp[childIno] == gen {
				continue
			}
			s.seenStamp[childIno] = gen
			unique++
			e.addUse(User, child.owner, Inodes, 1)
			e.addUse(Group, child.group, Inodes, 1)
			e.addUse(Tree, child.tree, Inodes, 1)
			if child.size != 0 {
				e.addUse(User, child.owner, Bytes, child.size)
				e.addUse(Group, child.group, Bytes, child.size)
				e.addUse(Tree, child.tree, Bytes, child.size)
			}
			rep.Bytes += child.size
		}
	}

	for _, n := range e.live {
		if s.entryStamp[n.id] != gen || s.entryCount[n.id] == 0 {
			rep.Problems = append(rep.Problems, fmt.Sprintf("inode %d 不可达", n.id))
			continue
		}
		if got := int(s.entryCount[n.id]); got != len(n.locs) {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"inode %d 有 %d 个目录项，但记录了 %d 个位置", n.id, got, len(n.locs)))
		}
		for _, l := range n.locs {
			p := e.nodes[l.parent]
			if p == nil || p.dir[l.name] != n.id {
				rep.Problems = append(rep.Problems, fmt.Sprintf(
					"inode %d 的位置记录 %d/%q 不存在", n.id, l.parent, l.name))
			}
		}
		mounted := false
		if m, isMount := e.mounts[n.id]; isMount {
			mounted = true
			if n.tree != m {
				rep.Problems = append(rep.Problems, fmt.Sprintf(
					"目录树根 inode %d 的树标签为 %q，挂了树 %q",
					n.id, e.acct(Tree, n.tree).ID, e.acct(Tree, m).ID))
			}
		}
		// Every entry must sit in the inode's tree: the engine keeps all hard
		// links of an inode inside one tree, so each entry's inherited label
		// must agree with the stored label. A tree root is the exception: its
		// own entry lives in the parent tree while the root itself belongs to
		// the tree it mounts.
		reported := false
		for _, l := range n.locs {
			if mounted {
				break
			}
			p := e.nodes[l.parent]
			if p == nil {
				continue
			}
			want, ok := getTree(p.id)
			if !ok {
				rep.Problems = append(rep.Problems, fmt.Sprintf(
					"inode %d 的父目录 %d 不可达", n.id, p.id))
				continue
			}
			if n.tree != want && !reported {
				reported = true
				rep.Problems = append(rep.Problems, fmt.Sprintf(
					"inode %d 的树标签为 %q，其在目录 %d/%q 下按继承应为 %q",
					n.id, e.acct(Tree, n.tree).ID, l.parent, l.name, e.acct(Tree, want).ID))
			}
		}
	}

	e.compareUsage(&rep)
	rep.Nodes = unique
	rep.OK = len(rep.Problems) == 0
	sortProblems(&rep)
	return rep
}

func growInt32(b []int32, n int) []int32 {
	if cap(b) < n {
		return make([]int32, n)
	}
	return b[:n]
}

func sortProblems(rep *VerifyReport) {
	if rep.OK || len(rep.Problems) < 2 {
		return
	}
	sort.Strings(rep.Problems)
	if len(rep.Problems) > 50 {
		rep.Problems = append(rep.Problems[:50],
			fmt.Sprintf("……另有 %d 个问题", len(rep.Problems)-50))
	}
}

func (e *Engine) resetUsageScratch() {
	for k := range e.scratch.usage {
		for d := range e.accts {
			if cap(e.scratch.usage[k][d]) < len(e.accts[d]) {
				e.scratch.usage[k][d] = make([]int64, len(e.accts[d]))
				continue
			}
			e.scratch.usage[k][d] = e.scratch.usage[k][d][:len(e.accts[d])]
			for i := range e.scratch.usage[k][d] {
				e.scratch.usage[k][d][i] = 0
			}
		}
	}
}

func (e *Engine) addUse(dim Dimension, idx int, k Kind, n int64) {
	if idx >= 0 && idx < len(e.scratch.usage[k][dim]) {
		e.scratch.usage[k][dim][idx] += n
	}
}

func (e *Engine) compareUsage(rep *VerifyReport) {
	rep.Accounts = e.CompareUsageInto(rep.Problems)
	rep.OK = len(rep.Problems) == 0
}

// CompareUsageInto checks every account's running counters against the
// scratch totals left by VerifyScan/VerifyState, and checks that usage never
// exceeds a hard limit and is never negative. It appends problems and returns
// the number of accounts checked. Callers hold the write lock or the verify
// lock.
func (e *Engine) CompareUsageInto(problems []string) int {
	checked := 0
	for _, dim := range AllDimensions {
		for i, a := range e.accts[dim] {
			checked++
			for _, k := range AllKinds {
				if got, want := a.Used[k], e.scratch.usage[k][dim][i]; got != want {
					problems = append(problems, fmt.Sprintf(
						"%s %q 的%s用量为 %d，逐文件扫描得到 %d", dim, a.ID, k.Label(), got, want))
				}
				if h := a.Limits[k].Hard; h > 0 && a.Used[k] > h {
					problems = append(problems, fmt.Sprintf(
						"%s %q 的%s用量 %d 超过硬限额 %d", dim, a.ID, k.Label(), a.Used[k], h))
				}
				if a.Used[k] < 0 {
					problems = append(problems, fmt.Sprintf(
						"%s %q 的%s用量为负 %d", dim, a.ID, k.Label(), a.Used[k]))
				}
			}
		}
	}
	return checked
}

// VerifyDenial checks one recorded refusal against the limit snapshot stored
// with it. It returns a problem description, or "" when the refusal is
// arithmetically sound.
func VerifyDenial(d Denial) string {
	if d.Demand <= 0 {
		return fmt.Sprintf("拒绝记录 #%d 的请求量为 %d，不是一次超限请求", d.Seq, d.Demand)
	}
	if d.Limit <= 0 {
		return fmt.Sprintf("拒绝记录 #%d 的生效限额为 %d", d.Seq, d.Limit)
	}
	if d.Used < 0 {
		return fmt.Sprintf("拒绝记录 #%d 的已用量为负 %d", d.Seq, d.Used)
	}
	if d.Used+d.Demand <= d.Limit {
		return fmt.Sprintf("拒绝记录 #%d 并未超限：已用 %d + 请求 %d <= 限额 %d",
			d.Seq, d.Used, d.Demand, d.Limit)
	}
	if d.Shortfall != d.Used+d.Demand-d.Limit {
		return fmt.Sprintf("拒绝记录 #%d 的缺口 %d 与算式不一致", d.Seq, d.Shortfall)
	}
	soft, hard := d.SoftAt, d.HardAt
	switch {
	case hard > 0 && d.Limit == hard && !d.GraceExpired:
		// Refused by the hard limit.
	case d.GraceExpired && soft > 0 && d.Limit == soft && (hard == 0 || soft < hard):
		// Refused by the soft limit after its grace period ran out.
	default:
		return fmt.Sprintf("拒绝记录 #%d 的生效限额 %d（宽限期已过=%v）与软/硬限额 %d/%d 不匹配",
			d.Seq, d.Limit, d.GraceExpired, soft, hard)
	}
	if d.Written < 0 || d.Written > d.Requested {
		return fmt.Sprintf("拒绝记录 #%d 的实际写入 %d 不在 [0, %d] 内", d.Seq, d.Written, d.Requested)
	}
	return ""
}

type verifyScratch struct {
	treeStamp  []uint32
	treeLabel  []int32
	entryStamp []uint32
	entryCount []int32
	seenStamp  []uint32
	gen        uint32
	stack      []NodeID
	usage      [2][3][]int64
}
