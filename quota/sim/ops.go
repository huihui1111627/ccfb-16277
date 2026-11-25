package sim

import (
	"fmt"
	"sort"
	"time"

	"filequota/quota"
)

// ---- helpers --------------------------------------------------------------

func (r *Runner) count(op quota.Op) {
	r.out.Operations[string(op)]++
	r.lastOp = op
}

// randomDir returns any directory, including tree roots.
func (r *Runner) randomDir() *mnode {
	return r.dirs[r.rng.Intn(len(r.dirs))]
}

// randomPlace returns a directory that is safe to place new children in: any
// directory other than the file system root.
func (r *Runner) randomPlace() *mnode {
	for {
		d := r.randomDir()
		if d.ino != quota.RootInode {
			return d
		}
	}
}

func (r *Runner) randomChild(dir *mnode) (*mnode, string, bool) {
	if len(dir.kids) == 0 {
		return nil, "", false
	}
	names := make([]string, 0, len(dir.kids))
	for n := range dir.kids {
		names = append(names, n)
	}
	sort.Strings(names)
	name := names[r.rng.Intn(len(names))]
	return dir.kids[name], name, true
}

func (r *Runner) freeName(dir *mnode, prefix string) string {
	for {
		name := r.newName(prefix)
		if _, ok := dir.kids[name]; !ok {
			return name
		}
	}
}

func (r *Runner) randomFile() *mnode {
	if len(r.order) == 0 {
		return nil
	}
	for i := 0; i < 16; i++ {
		m := r.order[r.rng.Intn(len(r.order))]
		if m.kind == quota.File {
			return m
		}
	}
	for _, m := range r.order {
		if m.kind == quota.File {
			return m
		}
	}
	return nil
}

func (r *Runner) path(m *mnode) string {
	if m == nil {
		return ""
	}
	parts := make([]string, 0, 6)
	cur := m
	for depth := 0; cur != nil && depth < 64; depth++ {
		if cur.ino == quota.RootInode {
			break
		}
		parts = append(parts, cur.name)
		cur = r.nodes[cur.parent]
	}
	if len(parts) == 0 {
		return "/"
	}
	out := ""
	for i := len(parts) - 1; i >= 0; i-- {
		out += "/" + parts[i]
	}
	return out
}

func (r *Runner) inSubtree(root, cand *mnode) bool {
	cur := cand
	for depth := 0; cur != nil && depth < 512; depth++ {
		if cur == root {
			return true
		}
		if cur.ino == quota.RootInode {
			return false
		}
		cur = r.nodes[cur.parent]
	}
	return false
}

// subtreeCharge returns the inode and byte totals a destination tree would
// receive if root moved into it, and whether some inode below root also has a
// hard link outside the moved subtree (which makes the move structurally
// impossible).
func (r *Runner) subtreeCharge(root *mnode, srcParent *mnode, movedName string, dstTree int) (count, bytes int64, straddles bool) {
	if root.tree == dstTree {
		return 0, 0, false
	}
	seen := make(map[*mnode]bool, 8)
	entries := make(map[*mnode]int, 8)
	var walk func(m *mnode)
	walk = func(m *mnode) {
		if m.kind != quota.Dir {
			return
		}
		for _, k := range m.kids {
			if k.tree != root.tree {
				continue
			}
			entries[k]++
			if !seen[k] {
				seen[k] = true
				count++
				bytes += k.size
				walk(k)
			}
		}
	}
	seen[root] = true
	count++
	bytes += root.size
	walk(root)
	for m, n := range entries {
		// The moved inode's entry in the source directory is the one being
		// relocated; every other link must be inside the subtree.
		allowed := 0
		if m == root && srcParent != nil {
			for name, k := range srcParent.kids {
				if k == m && name == movedName {
					allowed = 1
					break
				}
			}
		}
		if n+allowed != m.links {
			straddles = true
			break
		}
	}
	return count, bytes, straddles
}

// movePrediction is what the destination tree check says should happen.
type movePrediction struct {
	structErr bool // multi-link file across trees
	refused   bool
	dim       quota.Dimension
	account   string
	count     int64
	bytes     int64
}

func (r *Runner) predictMove(m *mnode, srcParent *mnode, movedName string, dstTree int) movePrediction {
	if m.tree == dstTree {
		return movePrediction{}
	}
	if m.kind != quota.Dir && m.links > 1 {
		return movePrediction{structErr: true}
	}
	count, bytes, straddles := r.subtreeCharge(m, srcParent, movedName, dstTree)
	if straddles {
		return movePrediction{structErr: true}
	}
	ai := dstTree
	if room, limited := r.room(dTree, ai, quota.Inodes); limited && room < count {
		return movePrediction{refused: true, dim: quota.Tree, account: r.ids[dTree][ai], count: count, bytes: bytes}
	}
	if bytes > 0 {
		if room, limited := r.room(dTree, ai, quota.Bytes); limited && room < bytes {
			return movePrediction{refused: true, dim: quota.Tree, account: r.ids[dTree][ai], count: count, bytes: bytes}
		}
	}
	return movePrediction{count: count, bytes: bytes}
}

func (r *Runner) addTo(parent *mnode, m *mnode) {
	parent.kids[m.name] = m
	m.parent = parent.ino
}

// ---- create, mkdir, symlink ----------------------------------------------

func (r *Runner) opCreateFile() {
	r.count(quota.OpCreate)
	dir := r.randomPlace()
	owner, group := r.pickUser(), r.pickGroup()
	ownerIdx := r.idx[dUser][owner]
	groupIdx := r.idx[dGroup][group]
	treeIdx := dir.tree

	placed := &mnode{owner: ownerIdx, group: groupIdx, tree: treeIdx}
	wantDim, wantID, refuse := r.predictInodeCreate(placed)
	name := r.freeName(dir, "f")
	res, err := r.Eng.Create(owner, group, dir.ino, name)
	if refuse {
		if d := r.expectDenial(quota.OpCreate, err, wantDim, wantID, 0, 1); d != nil {
			if d.Path != r.pathOf(dir, name) {
				r.fail("第 %d 步：拒绝路径 %q，预期 %q", r.steps, d.Path, r.pathOf(dir, name))
			}
		}
		return
	}
	if err != nil {
		r.fail("第 %d 步：新建文件 %s 预测成功，实际 %v", r.steps, r.pathOf(dir, name), err)
		return
	}
	f := r.newNode(res.Ino, quota.File, owner, group, treeIdx, 1, dir.ino, name)
	r.addTo(dir, f)
	r.chargeUsage(f, 0, 1)
}

func (r *Runner) pathOf(dir *mnode, name string) string {
	p := r.path(dir)
	if p == "/" {
		return "/" + name
	}
	return p + "/" + name
}

func (r *Runner) opMkdir() {
	r.count(quota.OpMkdir)
	dir := r.randomPlace()
	owner, group := r.pickUser(), r.pickGroup()
	ownerIdx := r.idx[dUser][owner]
	groupIdx := r.idx[dGroup][group]
	treeID := ""
	treeIdx := dir.tree
	// Occasionally retry mounting an existing tree: the engine must refuse it
	// structurally because a tree has exactly one root. The set of trees stays
	// at the configured size, like an office with a fixed number of accounts.
	if r.rng.Intn(8) == 0 {
		treeID = r.ids[dTree][r.rng.Intn(len(r.ids[dTree]))]
		if _, mounted := r.treeRootByID(treeID); mounted {
			name := r.freeName(dir, "d")
			_, err := r.Eng.Mkdir(owner, group, dir.ino, name, treeID)
			if _, ok := err.(*quota.OpError); !ok {
				r.fail("第 %d 步：重复挂载目录树 %q 应被结构拒绝，实际 %v", r.steps, treeID, err)
			}
			return
		}
		treeIdx = r.mustIdx(quota.Tree, treeID)
	}
	placed := &mnode{owner: ownerIdx, group: groupIdx, tree: treeIdx}
	wantDim, wantID, refuse := r.predictInodeCreate(placed)
	name := r.freeName(dir, "d")
	res, err := r.Eng.Mkdir(owner, group, dir.ino, name, treeID)
	if refuse {
		r.expectDenial(quota.OpMkdir, err, wantDim, wantID, 0, 1)
		return
	}
	if err != nil {
		r.fail("第 %d 步：新建目录 %s 预测成功，实际 %v", r.steps, r.pathOf(dir, name), err)
		return
	}
	d := r.newNode(res.Ino, quota.Dir, owner, group, treeIdx, 1, dir.ino, name)
	r.addTo(dir, d)
	if treeID != "" {
		r.mount[res.Ino] = treeIdx
		r.treeRoot[treeIdx] = res.Ino
	}
	r.chargeUsage(d, 0, 1)
}

func (r *Runner) treeRootByID(id string) (quota.NodeID, bool) {
	if id == quota.RootTreeID {
		// Mounted implicitly by the file system root.
		return quota.RootInode, true
	}
	ai, ok := r.idx[dTree][id]
	if !ok {
		return 0, false
	}
	ino, ok := r.treeRoot[ai]
	return ino, ok
}

func (r *Runner) opSymlink() {
	r.count(quota.OpSymlink)
	dir := r.randomPlace()
	owner, group := r.pickUser(), r.pickGroup()
	placed := &mnode{owner: r.idx[dUser][owner], group: r.idx[dGroup][group], tree: dir.tree}
	wantDim, wantID, refuse := r.predictInodeCreate(placed)
	name := r.freeName(dir, "l")
	res, err := r.Eng.Symlink(owner, group, dir.ino, name, "../target")
	if refuse {
		r.expectDenial(quota.OpSymlink, err, wantDim, wantID, 0, 1)
		return
	}
	if err != nil {
		r.fail("第 %d 步：新建符号链接 %s 预测成功，实际 %v", r.steps, r.pathOf(dir, name), err)
		return
	}
	l := r.newNode(res.Ino, quota.Symlink, owner, group, dir.tree, 1, dir.ino, name)
	l.size = 0
	r.addTo(dir, l)
	r.chargeUsage(l, 0, 1)
}

// ---- write and truncate ---------------------------------------------------

func (r *Runner) opWrite() {
	r.count(quota.OpWrite)
	f := r.randomFile()
	if f == nil {
		return
	}
	var n int64
	switch r.rng.Intn(100) {
	case 0:
		n = 0
	case 1:
		n = 1 + r.rng.Int63n(256<<10)
	default:
		n = 1 + r.rng.Int63n(40<<10)
	}
	fits, bd, bid, limited := r.predictGrow(f, n)
	res, err := r.Eng.Write(f.ino, n)
	if !limited || fits >= n {
		if err != nil {
			r.fail("第 %d 步：写入 %d 字节到 %s 预测成功，实际 %v", r.steps, n, r.path(f), err)
			return
		}
		if res.Written != n {
			r.fail("第 %d 步：写入成功但报告写入 %d，请求 %d", r.steps, res.Written, n)
		}
	} else {
		if err == nil {
			r.fail("第 %d 步：写入 %d 字节到 %s 预测只能写 %d，实际全部写入", r.steps, n, r.path(f), fits)
		} else if res.Written != fits {
			r.fail("第 %d 步：写入预测写入 %d 字节，实际 %d", r.steps, fits, res.Written)
			r.expectDenial(quota.OpWrite, err, bd, bid, 0, n)
		} else {
			if d := r.expectDenial(quota.OpWrite, err, bd, bid, 0, n); d != nil {
				if d.Written != fits || d.Requested != n {
					r.fail("第 %d 步：拒绝记录写入 %d/%d，预期 %d/%d", r.steps, d.Written, d.Requested, fits, n)
				}
			}
			r.out.Clipped++
		}
	}
	f.size += res.Written
	r.chargeUsage(f, res.Written, 0)
	r.out.Written += res.Written
}

func (r *Runner) opTruncate() {
	r.count(quota.OpTruncate)
	f := r.randomFile()
	if f == nil {
		return
	}
	switch r.rng.Intn(10) {
	case 0:
		target := int64(0)
		if _, err := r.Eng.Truncate(f.ino, target); err != nil {
			r.fail("第 %d 步：截断到 0 失败: %v", r.steps, err)
			return
		}
		r.chargeUsage(f, target-f.size, 0)
		f.size = target
	case 1, 2:
		target := f.size
		if _, err := r.Eng.Truncate(f.ino, target); err != nil {
			r.fail("第 %d 步：截断到当前大小失败: %v", r.steps, err)
		}
	case 3, 4, 5, 6:
		if f.size == 0 {
			return
		}
		target := r.rng.Int63n(f.size + 1)
		res, err := r.Eng.Truncate(f.ino, target)
		if err != nil {
			r.fail("第 %d 步：缩小截断到 %d 失败: %v", r.steps, target, err)
			return
		}
		if res.Freed != f.size-target {
			r.fail("第 %d 步：缩小截断释放 %d，预期 %d", r.steps, res.Freed, f.size-target)
		}
		r.chargeUsage(f, target-f.size, 0)
		f.size = target
	default:
		grow := 1 + r.rng.Int63n(128<<10)
		target := f.size + grow
		fits, bd, bid, limited := r.predictGrow(f, grow)
		res, err := r.Eng.Truncate(f.ino, target)
		if !limited || fits >= grow {
			if err != nil {
				r.fail("第 %d 步：截断增长预测成功，实际 %v", r.steps, err)
				return
			}
			if res.Size != target {
				r.fail("第 %d 步：截断后大小 %d，预期 %d", r.steps, res.Size, target)
			}
		} else {
			if err == nil {
				r.fail("第 %d 步：截断增长预测部分完成，实际全部完成", r.steps)
			} else {
				if res.Size != f.size+fits {
					r.fail("第 %d 步：截断增长预测大小 %d，实际 %d", r.steps, f.size+fits, res.Size)
				}
				r.expectDenial(quota.OpTruncate, err, bd, bid, 0, grow)
			}
		}
		r.chargeUsage(f, res.Size-f.size, 0)
		f.size = res.Size
	}
}

// ---- delete ---------------------------------------------------------------

func (r *Runner) opDelete() {
	r.count(quota.OpDelete)
	dir := r.randomDir()
	child, name, ok := r.randomChild(dir)
	if !ok {
		return
	}
	wantStruct := child.kind == quota.Dir && len(child.kids) > 0
	res, err := r.Eng.Delete(dir.ino, name)
	if wantStruct {
		if _, ok := err.(*quota.OpError); !ok {
			r.fail("第 %d 步：删除非空目录 %s 应被结构拒绝，实际 %v", r.steps, r.path(child), err)
		}
		return
	}
	if err != nil {
		r.fail("第 %d 步：删除 %s 预测成功，实际 %v", r.steps, r.path(child), err)
		return
	}
	wantFreed := int64(0)
	if child.links == 1 {
		wantFreed = child.size
	}
	if res.Freed != wantFreed {
		r.fail("第 %d 步：删除 %s 释放 %d，预期 %d", r.steps, r.path(child), res.Freed, wantFreed)
	}
	delete(dir.kids, name)
	child.links--
	if child.links == 0 {
		if t, wasRoot := r.mount[child.ino]; wasRoot {
			delete(r.treeRoot, t)
		}
		r.chargeUsage(child, -child.size, -1)
		r.dropNode(child)
	}
}

// ---- move -----------------------------------------------------------------

func (r *Runner) opMove() {
	r.count(quota.OpMove)
	src := r.randomDir()
	child, name, ok := r.randomChild(src)
	if !ok {
		return
	}
	dst := r.randomDir()
	if dst == src || dst.ino == child.ino || (child.kind == quota.Dir && r.inSubtree(child, dst)) {
		return
	}
	if _, exists := dst.kids[name]; exists {
		return
	}
	pred := r.predictMove(child, src, name, dst.tree)
	_, err := r.Eng.Move(src.ino, name, dst.ino, name)
	if pred.structErr {
		if _, ok := err.(*quota.OpError); !ok {
			r.fail("第 %d 步：跨树移动多链接文件 %s 应被结构拒绝，实际 %v", r.steps, r.path(child), err)
		}
		return
	}
	if pred.refused {
		if d := r.expectDenial(quota.OpMove, err, pred.dim, pred.account, 0, 0); d != nil {
			want := pred.bytes
			if d.Kind == quota.Inodes {
				want = pred.count
			}
			if d.Demand != want || d.Requested != want {
				r.fail("第 %d 步：移动拒绝请求 %d/%d，预期 %d", r.steps, d.Demand, d.Requested, want)
			}
		}
		return
	}
	if err != nil {
		r.fail("第 %d 步：移动 %s 预测成功，实际 %v", r.steps, r.path(child), err)
		return
	}
	// Charge transfer: the old tree loses the moved subtree, the new tree
	// gains it. Nested tree roots keep their own label and are skipped.
	oldTree := child.tree
	for _, m := range r.subtreeNodes(child) {
		if m.tree != oldTree {
			continue
		}
		old := m.tree
		m.tree = dst.tree
		r.use[dTree][old].inodes--
		r.use[dTree][old].bytes -= m.size
		r.use[dTree][dst.tree].inodes++
		r.use[dTree][dst.tree].bytes += m.size
	}
	if _, wasRoot := r.mount[child.ino]; wasRoot {
		delete(r.mount, child.ino)
		delete(r.treeRoot, oldTree)
	}
	delete(src.kids, name)
	child.name = name
	dst.kids[name] = child
	child.parent = dst.ino
}

func (r *Runner) subtreeNodes(root *mnode) []*mnode {
	seen := make(map[*mnode]bool, 8)
	var out []*mnode
	var walk func(m *mnode)
	walk = func(m *mnode) {
		if seen[m] || m.tree != root.tree {
			return
		}
		seen[m] = true
		out = append(out, m)
		if m.kind == quota.Dir {
			for _, k := range m.kids {
				walk(k)
			}
		}
	}
	walk(root)
	return out
}

// ---- hard links -----------------------------------------------------------

func (r *Runner) opHardlink() {
	r.count(quota.OpHardlink)
	f := r.randomFile()
	if f == nil {
		return
	}
	cands := make([]*mnode, 0, 8)
	for _, d := range r.dirs {
		if d.tree == f.tree && d.ino != quota.RootInode {
			cands = append(cands, d)
		}
	}
	if len(cands) == 0 {
		return
	}
	dst := cands[r.rng.Intn(len(cands))]
	name := r.freeName(dst, "h")
	if _, err := r.Eng.Link(dst.ino, name, f.ino); err != nil {
		r.fail("第 %d 步：同树硬链接 %s -> %s 失败: %v", r.steps, r.path(f), r.pathOf(dst, name), err)
		return
	}
	f.links++
	dst.kids[name] = f
}

func (r *Runner) opCrossTreeLink() {
	r.count(quota.OpHardlink)
	f := r.randomFile()
	if f == nil {
		return
	}
	cands := make([]*mnode, 0, 8)
	for _, d := range r.dirs {
		if d.tree != f.tree && d.ino != quota.RootInode {
			cands = append(cands, d)
		}
	}
	if len(cands) == 0 {
		return
	}
	dst := cands[r.rng.Intn(len(cands))]
	name := r.freeName(dst, "x")
	_, err := r.Eng.Link(dst.ino, name, f.ino)
	if _, ok := err.(*quota.OpError); !ok {
		r.fail("第 %d 步：跨目录树硬链接应被结构拒绝，实际 %v", r.steps, err)
	}
}

// ---- ownership ------------------------------------------------------------

func (r *Runner) opChownUser() {
	r.count(quota.OpChownUser)
	f := r.randomFile()
	if f == nil {
		return
	}
	newOwner := r.pickUser()
	if newOwner == r.ids[dUser][f.owner] {
		return
	}
	ai := r.idx[dUser][newOwner]
	wantDim, wantID, refuse := r.predictTransfer(dUser, ai, f.size)
	_, err := r.Eng.ChownOwner(f.ino, newOwner)
	if refuse {
		r.expectDenial(quota.OpChownUser, err, wantDim, wantID, 0, 0)
		return
	}
	if err != nil {
		r.fail("第 %d 步：改属主 %s 预测成功，实际 %v", r.steps, r.path(f), err)
		return
	}
	r.use[dUser][f.owner].inodes--
	r.use[dUser][f.owner].bytes -= f.size
	r.use[dUser][ai].inodes++
	r.use[dUser][ai].bytes += f.size
	f.owner = ai
}

func (r *Runner) opChownGroup() {
	r.count(quota.OpChownGroup)
	f := r.randomFile()
	if f == nil {
		return
	}
	newGroup := r.pickGroup()
	if newGroup == r.ids[dGroup][f.group] {
		return
	}
	ai := r.idx[dGroup][newGroup]
	wantDim, wantID, refuse := r.predictTransfer(dGroup, ai, f.size)
	_, err := r.Eng.ChownGroup(f.ino, newGroup)
	if refuse {
		r.expectDenial(quota.OpChownGroup, err, wantDim, wantID, 0, 0)
		return
	}
	if err != nil {
		r.fail("第 %d 步：改属组 %s 预测成功，实际 %v", r.steps, r.path(f), err)
		return
	}
	r.use[dGroup][f.group].inodes--
	r.use[dGroup][f.group].bytes -= f.size
	r.use[dGroup][ai].inodes++
	r.use[dGroup][ai].bytes += f.size
	f.group = ai
}

func (r *Runner) predictTransfer(d, ai int, size int64) (quota.Dimension, string, bool) {
	if room, limited := r.room(d, ai, quota.Inodes); limited && room < 1 {
		return quota.Dimension(d), r.ids[d][ai], true
	}
	if size > 0 {
		if room, limited := r.room(d, ai, quota.Bytes); limited && room < size {
			return quota.Dimension(d), r.ids[d][ai], true
		}
	}
	return 0, "", false
}

// ---- administration -------------------------------------------------------

func (r *Runner) opSetLimits() {
	r.count(quota.OpSetLimits)
	switch r.rng.Intn(20) {
	case 0:
		// Bad request: hard limit below current usage must be refused.
		d := r.rng.Intn(3)
		if len(r.ids[d]) == 0 {
			return
		}
		ai := r.rng.Intn(len(r.ids[d]))
		used := r.use[d][ai].bytes
		if used == 0 {
			return
		}
		next := [2]quota.Limit{r.lim[d][ai].bytes, r.lim[d][ai].inodes}
		next[0].Hard = used - 1
		next[0].Soft = 0
		err := r.Eng.SetLimits(quota.Dimension(d), r.ids[d][ai], next, "admin-low")
		if err == nil {
			r.fail("第 %d 步：把硬限额降到 %d 低于用量 %d 应被拒绝", r.steps, used-1, used)
		}
		return
	}
	d := r.rng.Intn(3)
	if len(r.ids[d]) == 0 {
		return
	}
	ai := r.rng.Intn(len(r.ids[d]))
	cur := r.lim[d][ai]
	next := [2]quota.Limit{cur.bytes, cur.inodes}

	// Move a limit in a direction that stays above usage unless the test
	// wants a refusal.
	k := quota.Bytes
	if r.rng.Intn(2) == 0 {
		k = quota.Inodes
	}
	used := r.use[d][ai].bytes
	if k == quota.Inodes {
		used = r.use[d][ai].inodes
	}
	delta := int64(1 + r.rng.Intn(32<<10))
	if k == quota.Inodes {
		delta = int64(1 + r.rng.Intn(8))
	}
	grow := r.rng.Intn(3) != 0
	apply := func(l *quota.Limit, used int64, step int64) bool {
		if grow {
			l.Hard += step
			if l.Soft > 0 {
				cand := l.Soft + step/2
				if cand >= l.Hard {
					cand = l.Hard / 2
				}
				l.Soft = cand
			}
			return true
		}
		if l.Hard-step < used {
			return false
		}
		l.Hard -= step
		if l.Soft > l.Hard {
			l.Soft = 0
		}
		return true
	}
	// Both hard limits must stay at or above current usage; the engine
	// validates the pair, and the untouched limit may already sit exactly at
	// usage.
	if next[0].Hard > 0 && next[0].Hard < r.use[d][ai].bytes {
		return
	}
	if next[1].Hard > 0 && next[1].Hard < r.use[d][ai].inodes {
		return
	}
	if k == quota.Bytes {
		if !apply(&next[0], used, delta) {
			return
		}
	} else {
		if !apply(&next[1], used, delta) {
			return
		}
	}
	if next[0].Hard > 0 && next[0].Hard < r.use[d][ai].bytes {
		return
	}
	if next[1].Hard > 0 && next[1].Hard < r.use[d][ai].inodes {
		return
	}
	actor := fmt.Sprintf("管理员%d", r.rng.Intn(3)+1)
	if err := r.Eng.SetLimits(quota.Dimension(d), r.ids[d][ai], next, actor); err != nil {
		r.fail("第 %d 步：修改限额失败: %v", r.steps, err)
		return
	}
	r.lim[d][ai] = limits{bytes: next[0], inodes: next[1]}
	// Limits changed: re-derive the soft-effective cache and clocks exactly
	// as the engine does.
	r.refreshAccount(d, ai)
	r.out.AdminEdits++
}

func (r *Runner) opGrantGrace() {
	r.count(quota.OpGrantGrace)
	type cand struct {
		d  int
		ai int
	}
	cands := make([]cand, 0, 8)
	for d := 0; d < 3; d++ {
		for ai := 0; ai < len(r.ids[d]); ai++ {
			for k := 0; k < 2; k++ {
				if r.clk[d][ai][k].armed {
					cands = append(cands, cand{d, ai})
					break
				}
			}
		}
	}
	if len(cands) == 0 {
		return
	}
	c := cands[r.rng.Intn(len(cands))]
	extra := time.Duration(1 + r.rng.Int63n(int64(r.Opts.DefaultGrace)))
	actor := fmt.Sprintf("管理员%d", r.rng.Intn(3)+1)
	err := r.Eng.GrantGrace(quota.Dimension(c.d), r.ids[c.d][c.ai], extra, actor)
	if err != nil {
		r.fail("第 %d 步：延长宽限期失败: %v", r.steps, err)
		return
	}
	for k := 0; k < 2; k++ {
		clk := &r.clk[c.d][c.ai][k]
		if !clk.armed {
			continue
		}
		base := clk.deadline
		if base.Before(r.now()) {
			base = r.now()
		}
		clk.deadline = base.Add(extra)
	}
	r.out.AdminEdits++
}

// ---- audit ----------------------------------------------------------------

func (r *Runner) audit() {
	rep := r.Eng.VerifyScan()
	if !rep.OK {
		for _, p := range rep.Problems {
			r.fail("第 %d 步扫描: %s", r.steps, p)
		}
	}
	for _, dim := range quota.AllDimensions {
		d := int(dim)
		if got := r.Eng.AccountCount(dim); got != len(r.ids[d]) {
			r.fail("第 %d 步：%s 账户数 %d，模型 %d", r.steps, dim, got, len(r.ids[d]))
		}
		r.Eng.AccountsView(dim, func(v quota.AccountView) {
			r.checkAccount(dim, d, v)
		})
	}
	if r.steps%r.Opts.CheckEvery == 0 {
		r.fullCompare()
		r.out.FullChecks++
	}
}

// checkAccount compares one engine account with the model.
func (r *Runner) checkAccount(dim quota.Dimension, d int, v quota.AccountView) {
	ai, ok := r.idx[d][v.ID]
	if !ok {
		r.fail("第 %d 步：引擎出现未知账户 %s %q", r.steps, dim, v.ID)
		return
	}
	if v.BytesUsed != r.use[d][ai].bytes || v.InodesUsed != r.use[d][ai].inodes {
		r.fail("第 %d 步：%s %q 引擎用量 %d/%d，模型 %d/%d", r.steps, dim, v.ID,
			v.BytesUsed, v.InodesUsed, r.use[d][ai].bytes, r.use[d][ai].inodes)
	}
	if v.BytesLimitNow != r.ceiling(d, ai, quota.Bytes) {
		r.fail("第 %d 步：%s %q 的空间生效限额 %d，模型 %d", r.steps, dim, v.ID,
			v.BytesLimitNow, r.ceiling(d, ai, quota.Bytes))
	}
	if v.InodesLimitNow != r.ceiling(d, ai, quota.Inodes) {
		r.fail("第 %d 步：%s %q 的文件数生效限额 %d，模型 %d", r.steps, dim, v.ID,
			v.InodesLimitNow, r.ceiling(d, ai, quota.Inodes))
	}
	if v.BytesHard > 0 && v.BytesUsed > v.BytesHard {
		r.fail("第 %d 步：%s %q 空间用量 %d 超过硬限额 %d", r.steps, dim, v.ID, v.BytesUsed, v.BytesHard)
	}
	if v.InodesHard > 0 && v.InodesUsed > v.InodesHard {
		r.fail("第 %d 步：%s %q 文件数用量 %d 超过硬限额 %d", r.steps, dim, v.ID, v.InodesUsed, v.InodesHard)
	}
	if gv, mv := v.BytesGrace, r.modelGrace(d, ai, quota.Bytes); gv != mv {
		r.fail("第 %d 步：%s %q 空间宽限期状态 %+v，模型 %+v", r.steps, dim, v.ID, gv, mv)
	}
	if gv, mv := v.InodesGrace, r.modelGrace(d, ai, quota.Inodes); gv != mv {
		r.fail("第 %d 步：%s %q 文件数宽限期状态 %+v，模型 %+v", r.steps, dim, v.ID, gv, mv)
	}
}

func (r *Runner) limitNow(v quota.AccountView, k quota.Kind) int64 {
	if k == quota.Bytes {
		return v.BytesLimitNow
	}
	return v.InodesLimitNow
}

func (r *Runner) graceView(v quota.AccountView, k quota.Kind) quota.GraceView {
	if k == quota.Bytes {
		return v.BytesGrace
	}
	return v.InodesGrace
}

// modelGrace mirrors the engine's grace reporting from the model clock.
func (r *Runner) modelGrace(d, ai int, k quota.Kind) quota.GraceView {
	clk := r.clk[d][ai][k]
	if !clk.armed {
		return quota.GraceView{}
	}
	if r.now().Before(clk.deadline) {
		return quota.GraceView{Active: true, Remaining: clk.deadline.Sub(r.now()).Seconds(), Deadline: clk.deadline}
	}
	soft := r.soft[d][ai][k]
	used := r.use[d][ai].bytes
	if k == quota.Inodes {
		used = r.use[d][ai].inodes
	}
	if soft > 0 && used > soft {
		return quota.GraceView{Active: true, Expired: true, Deadline: clk.deadline}
	}
	return quota.GraceView{}
}

func (r *Runner) fullCompare() {
	seen := make(map[quota.NodeID]quota.NodeInfo, len(r.nodes))
	r.Eng.ScanInodes(func(n quota.NodeInfo) { seen[n.Ino] = n })
	if len(seen) != len(r.order) {
		r.fail("第 %d 步：引擎有 %d 个 inode，模型 %d", r.steps, len(seen), len(r.order))
	}
	for _, m := range r.order {
		n, ok := seen[m.ino]
		if !ok {
			r.fail("第 %d 步：模型的 inode %d 不在引擎中", r.steps, m.ino)
			continue
		}
		if n.Kind != m.kind || n.Size != m.size || n.Links != m.links ||
			n.Owner != r.ids[dUser][m.owner] || n.Group != r.ids[dGroup][m.group] || n.Tree != r.ids[dTree][m.tree] {
			r.fail("第 %d 步：inode %d 引擎 %s/%d/%d链接 %s/%s/%s，模型 %s/%d/%d链接 %s/%s/%s",
				r.steps, m.ino, n.Kind, n.Size, n.Links, n.Owner, n.Group, n.Tree,
				m.kind, m.size, m.links, r.ids[dUser][m.owner], r.ids[dGroup][m.group], r.ids[dTree][m.tree])
		}
	}
	for ino := range seen {
		if _, ok := r.nodes[ino]; !ok {
			r.fail("第 %d 步：引擎的 inode %d 不在模型中", r.steps, ino)
		}
	}
	if rep := r.Eng.VerifyState(); !rep.OK {
		for _, p := range rep.Problems {
			r.fail("第 %d 步结构: %s", r.steps, p)
		}
	}
	// Directory entries must match the model exactly.
	for _, d := range r.dirs {
		dirs := r.Eng.Dirents(d.ino)
		if len(dirs) != len(d.kids) {
			r.fail("第 %d 步：目录 %s 有 %d 个目录项，模型 %d", r.steps, r.path(d), len(dirs), len(d.kids))
			continue
		}
		for _, de := range dirs {
			m, ok := d.kids[de.Name]
			if !ok || m.ino != de.Ino {
				r.fail("第 %d 步：目录 %s 的目录项 %q -> %d 与模型不符", r.steps, r.path(d), de.Name, de.Ino)
			}
		}
	}
}

// ---- denial validation ----------------------------------------------------

// expectDenial validates a refusal against the pre-operation model. Passing
// wantID == "" accepts any account (the operation's own preconditions were
// not modelled precisely enough) but still checks the arithmetic.
func (r *Runner) expectDenial(op quota.Op, err error, wantDim quota.Dimension, wantID string, _, demand int64) *quota.Denial {
	qe, ok := err.(*quota.QuotaError)
	if !ok {
		r.fail("第 %d 步：%s 预期配额拒绝，实际 %T（%v）", r.steps, op, err, err)
		return nil
	}
	d := qe.Denial
	if bad := quota.VerifyDenial(d); bad != "" {
		r.fail("第 %d 步：%s", r.steps, bad)
	}
	if d.Op != op {
		r.fail("第 %d 步：拒绝记录操作 %s，预期 %s", r.steps, d.Op, op)
	}
	if wantID == "" {
		return &d
	}
	if d.Dim != wantDim || d.Account != wantID {
		r.fail("第 %d 步：%s 拒绝来自 %s %q，模型预测 %s %q", r.steps, op, d.Dim, d.Account, wantDim, wantID)
	}
	d2 := int(wantDim)
	ai := r.idx[d2][wantID]
	used := r.use[d2][ai].bytes
	if d.Kind == quota.Inodes {
		used = r.use[d2][ai].inodes
	}
	// The engine records usage after any partial effect of the same
	// operation, so a clipped write adds its Written bytes to the pre-state.
	if d.Kind == quota.Bytes {
		used += d.Written
	}
	if d.Used != used {
		r.fail("第 %d 步：%s 拒绝记录已用 %d，操作前模型 %d + 已写入 %d", r.steps, op, d.Used, used-d.Written, d.Written)
	}
	if lim := r.ceiling(d2, ai, d.Kind); d.Limit != lim {
		r.fail("第 %d 步：%s 拒绝记录限额 %d，模型 %d", r.steps, op, d.Limit, lim)
	}
	// Every refusal must really be over the ceiling in force. Write and
	// grow-truncate report the usage before the operation started, so account
	// for the bytes that did fit.
	effUsed := d.Used
	if d.Written > 0 && d.Kind == quota.Bytes {
		effUsed = d.Used + d.Written
	}
	if effUsed+d.Demand-d.Written <= d.Limit && d.Demand > 0 {
		r.fail("第 %d 步：%s 拒绝记录并未超限（已用 %d + 请求 %d - 写入 %d <= 限额 %d）",
			r.steps, op, d.Used, d.Demand, d.Written, d.Limit)
	}
	return &d
}

func (r *Runner) predictInodeCreate(m *mnode) (quota.Dimension, string, bool) {
	for _, d := range r.chain() {
		ai := r.acctIndex(m, d)
		if room, limited := r.room(d, ai, quota.Inodes); limited && room < 1 {
			return quota.Dimension(d), r.ids[d][ai], true
		}
	}
	return 0, "", false
}

func (r *Runner) predictGrow(m *mnode, want int64) (int64, quota.Dimension, string, bool) {
	fits := want
	limited := false
	var bd quota.Dimension
	var bid string
	for _, d := range r.chain() {
		ai := r.acctIndex(m, d)
		room, has := r.room(d, ai, quota.Bytes)
		if !has {
			continue
		}
		limited = true
		if room < 0 {
			room = 0
		}
		if room < fits {
			fits = room
			bd = quota.Dimension(d)
			bid = r.ids[d][ai]
		}
	}
	return fits, bd, bid, limited
}
