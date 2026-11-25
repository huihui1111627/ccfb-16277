// Package quota implements an in-memory filesystem quota engine.
//
// A live inode is charged simultaneously against three independent
// dimensions:
//
//   - its owner user account,
//   - its owning group account,
//   - the directory tree it belongs to.
//
// Each account has a soft and a hard limit for both bytes and inode count.
// Usage above the soft limit is tolerated while a grace period runs; once the
// grace period expires and the account is still above the soft limit, the
// soft limit becomes the enforced ceiling. Usage above the enforced ceiling
// is refused outright.
//
// Directory trees are inherited by directory ID: a directory created with a
// tree ID is the root of that tree, and every inode below it belongs to that
// tree until another tree root is met. Moving an inode into a directory of a
// different tree transfers its charge, provided the destination tree can
// absorb it.
//
// Hard links add directory entries without adding quota usage: the inode is
// already charged. To keep "the tree an inode belongs to" well defined (and
// therefore independently scannable), every inode lives in exactly one tree;
// hard links into a different tree, and cross-tree moves of an inode that has
// more than one link, are structural errors and are refused.
//
// File system operations are simulated in memory; no real files are touched.
package quota

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// RootTreeID is the implicit tree account for entries that are below no
// explicit tree root.
const RootTreeID = "root"

// RootInode is the reserved inode of the simulated file system root. It is
// not charged against any account.
const RootInode NodeID = 0

// LogLimit bounds the in-memory refusal log.
const LogLimit = 20000

// AuditLimit bounds the administrative audit log.
const AuditLimit = 20000

// AccountSpec configures an account at startup.
type AccountSpec struct {
	ID     string
	Bytes  Limit
	Inodes Limit
	Grace  time.Duration
}

// Config configures an engine.
type Config struct {
	Users  []AccountSpec
	Groups []AccountSpec
	Trees  []AccountSpec
}

type loc struct {
	parent NodeID
	name   string
}

// node is one inode. A directory's dir map is the authoritative set of its
// directory entries. Hard links make several directory entries point at one
// node, but the inode is charged exactly once.
type node struct {
	id     NodeID
	kind   NodeKind
	size   int64
	owner  int // index into engine.accts[User]
	group  int // index into engine.accts[Group]
	tree   int // index into engine.accts[Tree]
	dir    map[string]NodeID
	locs   []loc
	target string
	idx    int // index into engine.live
}

// Dirent is one directory entry.
type Dirent struct {
	Name string `json:"name"`
	Ino  NodeID `json:"ino"`
}

// NodeInfo is a read-only copy of an inode.
type NodeInfo struct {
	Ino    NodeID   `json:"ino"`
	Kind   NodeKind `json:"kind"`
	Size   int64    `json:"size"`
	Owner  string   `json:"owner"`
	Group  string   `json:"group"`
	Tree   string   `json:"tree"`
	Links  int      `json:"links"`
	Target string   `json:"target,omitempty"`
}

// Engine is the quota simulator. All exported methods are safe for
// concurrent use.
type Engine struct {
	mu sync.RWMutex

	now          time.Time
	defaultGrace time.Duration

	accts   [3][]*Account
	acctIdx [3]map[string]int

	verifyMu sync.Mutex
	scratch  verifyScratch

	nodes map[NodeID]*node
	live  []*node // excludes the root; used for scans and verification
	// mounts maps the inode of a tree root directory to its tree index.
	mounts map[NodeID]int
	// treeRoot maps a tree index to the inode of its root directory.
	treeRoot map[int]NodeID

	nextIno NodeID

	denials      ring[Denial]
	denialsTotal int64
	denialsLost  int64

	activity      ring[ActivityEntry]
	activityTotal int64

	nextSeq int64
}

// New creates an empty engine at simulated time now.
func New(cfg Config, now time.Time, defaultGrace time.Duration) (*Engine, error) {
	if defaultGrace <= 0 {
		defaultGrace = DefaultGrace
	}
	e := &Engine{
		now:          now,
		defaultGrace: defaultGrace,
		nodes:        make(map[NodeID]*node),
		mounts:       make(map[NodeID]int),
		treeRoot:     make(map[int]NodeID),
	}
	for d := range e.acctIdx {
		e.acctIdx[d] = make(map[string]int)
	}
	rootTree := e.ensureIndex(Tree, RootTreeID)
	e.nodes[RootInode] = &node{id: RootInode, kind: Dir, tree: rootTree, dir: make(map[string]NodeID)}
	if err := e.load(User, cfg.Users); err != nil {
		return nil, err
	}
	if err := e.load(Group, cfg.Groups); err != nil {
		return nil, err
	}
	if err := e.load(Tree, cfg.Trees); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) load(dim Dimension, specs []AccountSpec) error {
	for _, s := range specs {
		if s.ID == "" {
			return errors.New("账户编号不能为空")
		}
		if s.ID == RootTreeID && dim != Tree {
			return fmt.Errorf("%s 账户不能使用保留编号 %q", dim, s.ID)
		}
		if err := validateLimits(s.Bytes, s.Inodes); err != nil {
			return fmt.Errorf("%s %q: %w", dim, s.ID, err)
		}
		a := e.acct(dim, e.ensureIndex(dim, s.ID))
		a.Limits[Bytes] = s.Bytes
		a.Limits[Inodes] = s.Inodes
		if s.Grace > 0 {
			a.Grace = s.Grace
		}
	}
	return nil
}

func validateLimits(limits ...Limit) error {
	for _, l := range limits {
		if l.Soft < 0 || l.Hard < 0 {
			return errors.New("限额不能为负数")
		}
		if l.Soft > 0 && l.Hard > 0 && l.Soft > l.Hard {
			return errors.New("软限额不能大于硬限额")
		}
	}
	return nil
}

func (e *Engine) ensureIndex(dim Dimension, id string) int {
	if i, ok := e.acctIdx[dim][id]; ok {
		return i
	}
	i := len(e.accts[dim])
	e.accts[dim] = append(e.accts[dim], &Account{ID: id, Dim: dim, Grace: e.defaultGrace})
	e.acctIdx[dim][id] = i
	return i
}

func (e *Engine) acct(dim Dimension, i int) *Account { return e.accts[dim][i] }

// Now returns simulated time.
func (e *Engine) Now() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.now
}

// SetNow advances simulated time. Time never runs backwards.
func (e *Engine) SetNow(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t.After(e.now) {
		e.now = t
	}
}

// Advance moves simulated time forward by d.
func (e *Engine) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	e.SetNow(e.Now().Add(d))
}

// ---- probes ---------------------------------------------------------------

type demand struct {
	dim   Dimension
	index int
	kind  Kind
	delta int64
}

type refusal struct {
	dim     Dimension
	index   int
	kind    Kind
	limit   int64
	expired bool
}

// probe evaluates demands against enforced limits in dimension order (user,
// group, tree) and returns the first refusal, or nil.
func (e *Engine) probe(demands []demand) *refusal {
	for _, dm := range demands {
		if dm.delta <= 0 {
			continue
		}
		a := e.acct(dm.dim, dm.index)
		limit := a.EffectiveLimit(dm.kind, e.now)
		if limit <= 0 {
			continue
		}
		if a.Used[dm.kind]+dm.delta <= limit {
			continue
		}
		return &refusal{
			dim:     dm.dim,
			index:   dm.index,
			kind:    dm.kind,
			limit:   limit,
			expired: limit != a.Limits[dm.kind].Hard,
		}
	}
	return nil
}

func (e *Engine) charge(dim Dimension, index int, k Kind, delta int64) {
	if delta == 0 {
		return
	}
	a := e.acct(dim, index)
	a.Used[k] += delta
	if a.Used[k] < 0 {
		a.Used[k] = 0
	}
	a.updateGrace(e.now)
}

func (e *Engine) dimIndex(nd *node, dim Dimension) int {
	switch dim {
	case Group:
		return nd.group
	case Tree:
		return nd.tree
	}
	return nd.owner
}

// byteRoom returns how many bytes can still be charged to the node's three
// accounts before a byte ceiling is hit.
func (e *Engine) byteRoom(nd *node) (int64, bool) {
	best := int64(math.MaxInt64)
	limited := false
	for _, dim := range AllDimensions {
		a := e.acct(dim, e.dimIndex(nd, dim))
		limit := a.EffectiveLimit(Bytes, e.now)
		if limit <= 0 {
			continue
		}
		limited = true
		room := limit - a.Used[Bytes]
		if room < 0 {
			room = 0
		}
		if room < best {
			best = room
		}
	}
	if !limited {
		return math.MaxInt64, false
	}
	return best, true
}

// byteBlocker returns the account that keeps a node from growing one more
// byte, or nil when no byte ceiling is exhausted.
func (e *Engine) byteBlocker(nd *node) *refusal {
	for _, dim := range AllDimensions {
		idx := e.dimIndex(nd, dim)
		a := e.acct(dim, idx)
		limit := a.EffectiveLimit(Bytes, e.now)
		if limit <= 0 || a.Used[Bytes] < limit {
			continue
		}
		return &refusal{dim: dim, index: idx, kind: Bytes, limit: limit,
			expired: limit != a.Limits[Bytes].Hard}
	}
	return nil
}

// ---- creation -------------------------------------------------------------

func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\x00")
}

func (e *Engine) lookup(parent NodeID, name string) (NodeID, bool) {
	p, ok := e.nodes[parent]
	if !ok || p.kind != Dir {
		return 0, false
	}
	id, ok := p.dir[name]
	return id, ok
}

// treeRootIno returns the inode of the directory that mounted a tree.
func (e *Engine) treeRootIno(treeID string) (NodeID, bool) {
	ti, ok := e.acctIdx[Tree][treeID]
	if !ok {
		return 0, false
	}
	ino, ok := e.treeRoot[ti]
	return ino, ok
}

func (e *Engine) validParent(parent NodeID) bool {
	p, ok := e.nodes[parent]
	return ok && p.kind == Dir
}

func (e *Engine) createNode(op Op, kind NodeKind, owner, group string, parent NodeID, name, treeID, target string) (Result, error) {
	if !e.validParent(parent) {
		return Result{}, &OpError{Op: op, Reason: "父目录不存在或不是目录", Ino: parent}
	}
	if !validName(name) {
		return Result{}, &OpError{Op: op, Reason: "非法文件名", Ino: parent}
	}
	if _, exists := e.lookup(parent, name); exists {
		return Result{}, &OpError{Op: op, Reason: "同名目录项已存在", Ino: parent}
	}
	tree := e.nodes[parent].tree
	if kind == Dir && treeID == RootTreeID {
		return Result{}, &OpError{Op: op, Reason: "root 目录树由文件系统根目录隐式挂载，不能再挂载", Ino: parent}
	}
	if kind == Dir && treeID != "" {
		if ino, ok := e.treeRootIno(treeID); ok {
			return Result{}, &OpError{Op: op, Reason: fmt.Sprintf("目录树 %q 已存在根目录", treeID), Ino: ino}
		}
		tree = e.ensureIndex(Tree, treeID)
	}
	ownerIdx := e.ensureIndex(User, owner)
	groupIdx := e.ensureIndex(Group, group)
	path := e.joinPath(parent, name)
	if r := e.probe([]demand{
		{User, ownerIdx, Inodes, 1},
		{Group, groupIdx, Inodes, 1},
		{Tree, tree, Inodes, 1},
	}); r != nil {
		return Result{}, e.refuse(pendingDenial{op: op, path: path, r: *r, used: e.acct(r.dim, r.index).Used[r.kind], demand: 1, requested: 1})
	}

	e.nextIno++
	n := &node{
		id:     e.nextIno,
		kind:   kind,
		owner:  ownerIdx,
		group:  groupIdx,
		tree:   tree,
		locs:   []loc{{parent: parent, name: name}},
		target: target,
	}
	if kind == Dir {
		n.dir = make(map[string]NodeID)
	}
	n.idx = len(e.live)
	e.live = append(e.live, n)
	e.nodes[n.id] = n
	e.nodes[parent].dir[name] = n.id
	if kind == Dir && treeID != "" {
		e.mounts[n.id] = tree
		e.treeRoot[tree] = n.id
	}
	e.charge(User, ownerIdx, Inodes, 1)
	e.charge(Group, groupIdx, Inodes, 1)
	e.charge(Tree, tree, Inodes, 1)
	return Result{Ino: n.id}, nil
}

// Create makes a regular file owned by owner/group inside parent.
func (e *Engine) Create(owner, group string, parent NodeID, name string) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.createNode(OpCreate, File, owner, group, parent, name, "", "")
}

// Mkdir makes a directory. When treeID is non-empty the new directory is the
// root of that tree; otherwise it inherits the parent's tree.
func (e *Engine) Mkdir(owner, group string, parent NodeID, name, treeID string) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.createNode(OpMkdir, Dir, owner, group, parent, name, treeID, "")
}

// Symlink makes a symbolic link.
func (e *Engine) Symlink(owner, group string, parent NodeID, name, target string) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.createNode(OpSymlink, Symlink, owner, group, parent, name, "", target)
}

// ---- refusals and data ----------------------------------------------------

type pendingDenial struct {
	op        Op
	ino       NodeID
	path      string
	r         refusal
	used      int64
	demand    int64
	requested int64
	written   int64
}

func (e *Engine) refuse(p pendingDenial) error {
	a := e.acct(p.r.dim, p.r.index)
	e.nextSeq++
	d := Denial{
		Seq:          e.nextSeq,
		At:           e.now,
		Op:           p.op,
		Ino:          p.ino,
		Path:         p.path,
		Dim:          p.r.dim,
		Account:      a.ID,
		Kind:         p.r.kind,
		Used:         p.used,
		Demand:       p.demand,
		Shortfall:    p.used + p.demand - p.r.limit,
		Requested:    p.requested,
		Written:      p.written,
		Limit:        p.r.limit,
		SoftAt:       a.Limits[p.r.kind].Soft,
		HardAt:       a.Limits[p.r.kind].Hard,
		GraceExpired: p.r.expired,
	}
	e.denialsTotal++
	e.denialsLost += e.denials.push(d, LogLimit)
	return &QuotaError{Denial: d}
}

// grow charges up to n bytes, clipped at the tightest available room. A
// non-nil refusal means the write was clipped; pre is the blocking account's
// usage before the operation for each dimension.
func (e *Engine) grow(nd *node, n int64) (written int64, r *refusal, pre [3]int64) {
	if n <= 0 {
		return 0, nil, pre
	}
	for _, dim := range AllDimensions {
		pre[dim] = e.acct(dim, e.dimIndex(nd, dim)).Used[Bytes]
	}
	room, limited := e.byteRoom(nd)
	written = n
	if limited && written > room {
		written = room
	}
	if written < 0 {
		written = 0
	}
	if written > 0 {
		nd.size += written
		for _, dim := range AllDimensions {
			e.charge(dim, e.dimIndex(nd, dim), Bytes, written)
		}
	}
	if written == n {
		return written, nil, pre
	}
	r = e.byteBlocker(nd)
	if r == nil {
		r = &refusal{dim: Tree, index: nd.tree, kind: Bytes}
	}
	return written, r, pre
}

// Write appends n bytes to a file. When a limit is hit the write is clipped
// and the returned Result reports the bytes actually written together with a
// *QuotaError.
func (e *Engine) Write(ino NodeID, n int64) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n < 0 {
		return Result{}, &OpError{Op: OpWrite, Reason: "写入长度不能为负", Ino: ino}
	}
	nd, ok := e.nodes[ino]
	if !ok || nd.kind != File {
		return Result{}, &OpError{Op: OpWrite, Reason: "不是普通文件", Ino: ino}
	}
	written, r, pre := e.grow(nd, n)
	res := Result{Ino: ino, Size: nd.size, Written: written}
	if r != nil {
		return res, e.refuse(pendingDenial{
			op: OpWrite, ino: ino, path: e.pathLocked(nd), r: *r,
			used: pre[r.dim], demand: n, requested: n, written: written,
		})
	}
	return res, nil
}

// Truncate sets a file's size. Shrinking frees usage immediately. Growing is
// clipped the same way as Write.
func (e *Engine) Truncate(ino NodeID, size int64) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if size < 0 {
		return Result{}, &OpError{Op: OpTruncate, Reason: "大小不能为负", Ino: ino}
	}
	nd, ok := e.nodes[ino]
	if !ok || nd.kind != File {
		return Result{}, &OpError{Op: OpTruncate, Reason: "不是普通文件", Ino: ino}
	}
	switch {
	case size == nd.size:
		return Result{Ino: ino, Size: nd.size}, nil
	case size < nd.size:
		freed := nd.size - size
		nd.size = size
		for _, dim := range AllDimensions {
			e.charge(dim, e.dimIndex(nd, dim), Bytes, -freed)
		}
		return Result{Ino: ino, Size: size, Freed: freed}, nil
	}
	delta := size - nd.size
	grown, r, pre := e.grow(nd, delta)
	res := Result{Ino: ino, Size: nd.size, Written: grown}
	if r != nil {
		return res, e.refuse(pendingDenial{
			op: OpTruncate, ino: ino, path: e.pathLocked(nd), r: *r,
			used: pre[r.dim], demand: delta, requested: delta, written: grown,
		})
	}
	return res, nil
}

// ---- removal and links ----------------------------------------------------

// Delete removes one directory entry. Usage is released when the last link
// disappears. Directories must be empty.
func (e *Engine) Delete(parent NodeID, name string) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.nodes[parent]
	if !ok || p.kind != Dir {
		return Result{}, &OpError{Op: OpDelete, Reason: "父目录不存在或不是目录", Ino: parent}
	}
	ino, ok := p.dir[name]
	if !ok {
		return Result{}, &OpError{Op: OpDelete, Reason: "目录项不存在", Ino: parent}
	}
	nd := e.nodes[ino]
	if nd.kind == Dir && len(nd.dir) > 0 {
		return Result{}, &OpError{Op: OpDelete, Reason: "目录非空", Ino: ino}
	}
	delete(p.dir, name)
	nd.locs = removeLoc(nd.locs, parent, name)
	res := Result{Ino: ino, Size: nd.size}
	if len(nd.locs) == 0 {
		res.Freed = nd.size
		e.charge(User, nd.owner, Inodes, -1)
		e.charge(Group, nd.group, Inodes, -1)
		e.charge(Tree, nd.tree, Inodes, -1)
		if nd.size > 0 {
			e.charge(User, nd.owner, Bytes, -nd.size)
			e.charge(Group, nd.group, Bytes, -nd.size)
			e.charge(Tree, nd.tree, Bytes, -nd.size)
		}
		if t, wasRoot := e.mounts[nd.id]; wasRoot {
			delete(e.treeRoot, t)
		}
		delete(e.mounts, nd.id)
		e.removeNode(nd)
	}
	return res, nil
}

func removeLoc(locs []loc, parent NodeID, name string) []loc {
	for i := range locs {
		if locs[i].parent == parent && locs[i].name == name {
			out := make([]loc, 0, len(locs)-1)
			out = append(out, locs[:i]...)
			out = append(out, locs[i+1:]...)
			return out
		}
	}
	return locs
}

func (e *Engine) removeNode(nd *node) {
	last := len(e.live) - 1
	if nd.idx != last {
		e.live[nd.idx] = e.live[last]
		e.live[nd.idx].idx = nd.idx
	}
	e.live = e.live[:last]
	delete(e.nodes, nd.id)
}

// Link creates an additional hard link. It adds no quota usage: the inode is
// already charged to its owner, group and tree. The link must be in the same
// directory tree, so that an inode belongs to exactly one tree.
func (e *Engine) Link(parent NodeID, name string, ino NodeID) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	nd, ok := e.nodes[ino]
	if !ok {
		return Result{}, &OpError{Op: OpHardlink, Reason: "源文件不存在", Ino: ino}
	}
	if nd.kind == Dir {
		return Result{}, &OpError{Op: OpHardlink, Reason: "不能给目录建硬链接", Ino: ino}
	}
	if !e.validParent(parent) {
		return Result{}, &OpError{Op: OpHardlink, Reason: "目标父目录不存在或不是目录", Ino: parent}
	}
	if !validName(name) {
		return Result{}, &OpError{Op: OpHardlink, Reason: "非法文件名", Ino: parent}
	}
	if _, exists := e.lookup(parent, name); exists {
		return Result{}, &OpError{Op: OpHardlink, Reason: "同名目录项已存在", Ino: parent}
	}
	if t := e.nodes[parent].tree; t != nd.tree {
		return Result{}, &OpError{Op: OpHardlink, Reason: "硬链接必须与文件在同一目录树内", Ino: ino}
	}
	e.nodes[parent].dir[name] = ino
	nd.locs = append(nd.locs, loc{parent: parent, name: name})
	return Result{Ino: ino, Size: nd.size}, nil
}

// ---- move and ownership ---------------------------------------------------

func (e *Engine) isDescendant(dir NodeID, candidate NodeID) bool {
	cur := candidate
	for depth := 0; depth < 4096; depth++ {
		if cur == dir {
			return true
		}
		if cur == RootInode {
			return false
		}
		n, ok := e.nodes[cur]
		if !ok || len(n.locs) == 0 {
			return false
		}
		cur = n.locs[0].parent
	}
	return false
}

// collectSubtree appends every inode in the subtree rooted at nd whose tree
// label equals want, each exactly once. Nested tree roots are pruned.
func (e *Engine) collectSubtree(nd *node, want int, seen map[NodeID]bool, out *[]*node) {
	if nd.tree != want || seen[nd.id] {
		return
	}
	seen[nd.id] = true
	*out = append(*out, nd)
	if nd.kind != Dir {
		return
	}
	names := make([]string, 0, len(nd.dir))
	for name := range nd.dir {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e.collectSubtree(e.nodes[nd.dir[name]], want, seen, out)
	}
}

// Move renames parent/name to newParent/newName, transferring usage between
// directory trees when the destination tree differs. If the destination tree
// cannot absorb the subtree the move is refused and nothing changes.
func (e *Engine) Move(parent NodeID, name string, newParent NodeID, newName string) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.validParent(parent) {
		return Result{}, &OpError{Op: OpMove, Reason: "源父目录不存在或不是目录", Ino: parent}
	}
	ino, ok := e.nodes[parent].dir[name]
	if !ok {
		return Result{}, &OpError{Op: OpMove, Reason: "源目录项不存在", Ino: parent}
	}
	if !e.validParent(newParent) {
		return Result{}, &OpError{Op: OpMove, Reason: "目标父目录不存在或不是目录", Ino: newParent}
	}
	if !validName(newName) {
		return Result{}, &OpError{Op: OpMove, Reason: "非法目标名", Ino: newParent}
	}
	if parent == newParent && name == newName {
		return Result{Ino: ino, Size: e.nodes[ino].size}, nil
	}
	if _, exists := e.lookup(newParent, newName); exists {
		return Result{}, &OpError{Op: OpMove, Reason: "目标目录项已存在", Ino: newParent}
	}
	nd := e.nodes[ino]
	if nd.kind == Dir && (newParent == ino || e.isDescendant(ino, newParent)) {
		return Result{}, &OpError{Op: OpMove, Reason: "不能把目录移动到自身子树内", Ino: ino}
	}
	oldTree := nd.tree
	newTree := e.nodes[newParent].tree
	if oldTree != newTree {
		if nd.kind != Dir && len(nd.locs) > 1 {
			return Result{}, &OpError{Op: OpMove, Reason: "有多个硬链接的文件不能跨目录树移动", Ino: ino}
		}
		var transfer []*node
		e.collectSubtree(nd, oldTree, make(map[NodeID]bool, 16), &transfer)
		// A moved directory carries every link of the inodes below it. If a
		// hard link of one of those inodes lives outside the moved subtree the
		// move would leave the inode straddling two trees, so refuse it.
		inside := make(map[NodeID]bool, len(transfer))
		for _, sub := range transfer {
			inside[sub.id] = true
		}
		for _, sub := range transfer {
			for _, l := range sub.locs {
				// The moved inode's own entry in the source directory is the
				// one being relocated. Every other link must travel with the
				// subtree, otherwise the inode would be split across trees.
				if sub == nd && l.parent == parent && l.name == name {
					continue
				}
				if !inside[l.parent] {
					return Result{}, &OpError{Op: OpMove,
						Reason: "子树内有文件在其他目录还有硬链接，跨目录树移动会把同一文件分到两棵树",
						Ino:    sub.id}
				}
			}
		}
		var bytes, count int64
		for _, sub := range transfer {
			bytes += sub.size
			count++
		}
		if r := e.probe([]demand{
			{Tree, newTree, Inodes, count},
			{Tree, newTree, Bytes, bytes},
		}); r != nil {
			requested := bytes
			if r.kind == Inodes {
				requested = count
			}
			return Result{}, e.refuse(pendingDenial{
				op: OpMove, ino: ino, path: e.pathLocked(nd), r: *r,
				used: e.acct(Tree, newTree).Used[r.kind], demand: demandFor(r.kind, bytes, count), requested: requested,
			})
		}
		for _, sub := range transfer {
			sub.tree = newTree
			e.charge(Tree, oldTree, Inodes, -1)
			e.charge(Tree, newTree, Inodes, 1)
			if sub.size != 0 {
				e.charge(Tree, oldTree, Bytes, -sub.size)
				e.charge(Tree, newTree, Bytes, sub.size)
			}
		}
		if _, wasRoot := e.mounts[nd.id]; wasRoot {
			delete(e.mounts, nd.id)
			delete(e.treeRoot, oldTree)
		}
	}
	delete(e.nodes[parent].dir, name)
	e.nodes[newParent].dir[newName] = ino
	nd.locs = replaceLoc(nd.locs, parent, name, newParent, newName)
	return Result{Ino: ino, Size: nd.size}, nil
}

func replaceLoc(locs []loc, oldParent NodeID, oldName string, newParent NodeID, newName string) []loc {
	for i := range locs {
		if locs[i].parent == oldParent && locs[i].name == oldName {
			locs[i] = loc{parent: newParent, name: newName}
			return locs
		}
	}
	return locs
}

func demandFor(k Kind, bytes, count int64) int64 {
	if k == Inodes {
		return count
	}
	return bytes
}

func (e *Engine) transferAccount(nd *node, dim Dimension, to int, op Op) error {
	if r := e.probe([]demand{{dim, to, Inodes, 1}, {dim, to, Bytes, nd.size}}); r != nil {
		requested := nd.size
		if r.kind == Inodes {
			requested = 1
		}
		return e.refuse(pendingDenial{
			op: op, ino: nd.id, path: e.pathLocked(nd), r: *r,
			used: e.acct(dim, to).Used[r.kind], demand: demandFor(r.kind, nd.size, 1), requested: requested,
		})
	}
	old := e.dimIndex(nd, dim)
	switch dim {
	case User:
		nd.owner = to
	case Group:
		nd.group = to
	}
	e.charge(dim, old, Inodes, -1)
	e.charge(dim, to, Inodes, 1)
	if nd.size != 0 {
		e.charge(dim, old, Bytes, -nd.size)
		e.charge(dim, to, Bytes, nd.size)
	}
	return nil
}

// ChownOwner changes a file's owner and transfers its usage.
func (e *Engine) ChownOwner(ino NodeID, newOwner string) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	nd, ok := e.nodes[ino]
	if !ok {
		return Result{}, &OpError{Op: OpChownUser, Reason: "文件不存在", Ino: ino}
	}
	idx := e.ensureIndex(User, newOwner)
	if idx == nd.owner {
		return Result{Ino: ino, Size: nd.size}, nil
	}
	if err := e.transferAccount(nd, User, idx, OpChownUser); err != nil {
		return Result{}, err
	}
	return Result{Ino: ino, Size: nd.size}, nil
}

// ChownGroup changes a file's group and transfers its usage.
func (e *Engine) ChownGroup(ino NodeID, newGroup string) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	nd, ok := e.nodes[ino]
	if !ok {
		return Result{}, &OpError{Op: OpChownGroup, Reason: "文件不存在", Ino: ino}
	}
	idx := e.ensureIndex(Group, newGroup)
	if idx == nd.group {
		return Result{Ino: ino, Size: nd.size}, nil
	}
	if err := e.transferAccount(nd, Group, idx, OpChownGroup); err != nil {
		return Result{}, err
	}
	return Result{Ino: ino, Size: nd.size}, nil
}

// ---- administration -------------------------------------------------------

// SetLimits changes an account's limits and records who did it. A hard limit
// may not be lowered below current usage, keeping the invariant that no
// account ever sits above its hard limit.
func (e *Engine) SetLimits(dim Dimension, id string, limits [2]Limit, actor string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if actor == "" {
		return errors.New("必须记录操作人")
	}
	if dim < User || dim > Tree {
		return errors.New("未知维度")
	}
	if err := validateLimits(limits[0], limits[1]); err != nil {
		return err
	}
	a := e.acct(dim, e.ensureIndex(dim, id))
	for _, k := range AllKinds {
		if h := limits[k].Hard; h > 0 && h < a.Used[k] {
			return fmt.Errorf("硬限额不能低于当前用量：%s 已用 %d", k.Label(), a.Used[k])
		}
	}
	var changes []string
	for _, k := range AllKinds {
		old, nw := a.Limits[k], limits[k]
		if old.Soft != nw.Soft {
			changes = append(changes, fmt.Sprintf("%s软限额 %d -> %d", k.Label(), old.Soft, nw.Soft))
		}
		if old.Hard != nw.Hard {
			changes = append(changes, fmt.Sprintf("%s硬限额 %d -> %d", k.Label(), old.Hard, nw.Hard))
		}
	}
	if len(changes) == 0 {
		return nil
	}
	a.Limits = limits
	a.updateGrace(e.now)
	e.recordActivity(OpSetLimits, dim, id, actor, changes)
	return nil
}

// GrantGrace extends a running grace period by extra. If the deadline has
// already passed, the extension counts from now, restoring the hard limit
// until the new deadline.
func (e *Engine) GrantGrace(dim Dimension, id string, extra time.Duration, actor string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if actor == "" {
		return errors.New("必须记录操作人")
	}
	if extra <= 0 {
		return errors.New("延长时间必须为正")
	}
	if dim < User || dim > Tree {
		return errors.New("未知维度")
	}
	a := e.acct(dim, e.ensureIndex(dim, id))
	var changes []string
	for _, k := range AllKinds {
		if !a.graceActive(k) {
			continue
		}
		a.extendGrace(k, e.now, extra)
		changes = append(changes, fmt.Sprintf("%s宽限期延长 %s", k.Label(), extra))
	}
	if len(changes) == 0 {
		return fmt.Errorf("%s %s 当前没有可延长的宽限期", dim, id)
	}
	e.recordActivity(OpGrantGrace, dim, id, actor, changes)
	return nil
}

// LimitsOf returns a copy of the configured limits.
func (e *Engine) LimitsOf(dim Dimension, id string) ([2]Limit, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	i, ok := e.acctIdx[dim][id]
	if !ok {
		return [2]Limit{}, false
	}
	return e.acct(dim, i).Limits, true
}

func (e *Engine) recordActivity(action Op, dim Dimension, id, actor string, changes []string) {
	e.nextSeq++
	e.activityTotal++
	e.activity.push(ActivityEntry{
		Seq:     e.nextSeq,
		At:      e.now,
		Actor:   actor,
		Action:  action,
		Dim:     dim,
		Account: id,
		Changes: changes,
	}, AuditLimit)
}

// ---- paths and inspection -------------------------------------------------

func (e *Engine) joinPath(parent NodeID, name string) string {
	p := e.dirPath(parent)
	if p == "/" {
		return "/" + name
	}
	return p + "/" + name
}

func (e *Engine) dirPath(ino NodeID) string {
	if ino == RootInode {
		return "/"
	}
	parts := make([]string, 0, 8)
	cur := ino
	for depth := 0; cur != RootInode && depth < 512; depth++ {
		nd, ok := e.nodes[cur]
		if !ok || len(nd.locs) == 0 {
			break
		}
		l := nd.locs[0]
		parts = append(parts, l.name)
		cur = l.parent
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return "/" + strings.Join(parts, "/")
}

func (e *Engine) pathLocked(nd *node) string {
	if len(nd.locs) == 0 {
		return ""
	}
	l := nd.locs[0]
	return e.joinPath(l.parent, l.name)
}

// Path returns the primary path of an inode.
func (e *Engine) Path(ino NodeID) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	nd, ok := e.nodes[ino]
	if !ok {
		return "", false
	}
	return e.pathLocked(nd), true
}

// Dirents lists a directory's entries sorted by name.
func (e *Engine) Dirents(ino NodeID) []Dirent {
	e.mu.RLock()
	defer e.mu.RUnlock()
	n, ok := e.nodes[ino]
	if !ok || n.kind != Dir {
		return nil
	}
	out := make([]Dirent, 0, len(n.dir))
	for name, id := range n.dir {
		out = append(out, Dirent{Name: name, Ino: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NodeInfo returns a copy of an inode.
func (e *Engine) NodeInfo(ino NodeID) (NodeInfo, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	n, ok := e.nodes[ino]
	if !ok {
		return NodeInfo{}, false
	}
	return e.info(n), true
}

func (e *Engine) info(n *node) NodeInfo {
	return NodeInfo{
		Ino:    n.id,
		Kind:   n.kind,
		Size:   n.size,
		Owner:  e.acct(User, n.owner).ID,
		Group:  e.acct(Group, n.group).ID,
		Tree:   e.acct(Tree, n.tree).ID,
		Links:  len(n.locs),
		Target: n.target,
	}
}

// ScanInodes calls fn once for every live inode while holding the read lock.
// The callback must not call back into the engine.
func (e *Engine) ScanInodes(fn func(NodeInfo)) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, n := range e.live {
		fn(e.info(n))
	}
}

// LiveNodes returns a snapshot of all non-root inodes.
func (e *Engine) LiveNodes() []NodeInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]NodeInfo, 0, len(e.live))
	for _, n := range e.live {
		out = append(out, e.info(n))
	}
	return out
}

// AccountView returns a snapshot of one account.
func (e *Engine) AccountView(dim Dimension, id string) (AccountView, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	i, ok := e.acctIdx[dim][id]
	if !ok {
		return AccountView{}, false
	}
	return e.view(e.acct(dim, i), i), true
}

func (e *Engine) view(a *Account, index int) AccountView {
	v := AccountView{
		ID:             a.ID,
		Dim:            a.Dim,
		BytesUsed:      a.Used[Bytes],
		InodesUsed:     a.Used[Inodes],
		BytesSoft:      a.Limits[Bytes].Soft,
		BytesHard:      a.Limits[Bytes].Hard,
		InodesSoft:     a.Limits[Inodes].Soft,
		InodesHard:     a.Limits[Inodes].Hard,
		BytesLimitNow:  a.EffectiveLimit(Bytes, e.now),
		InodesLimitNow: a.EffectiveLimit(Inodes, e.now),
		BytesGrace:     a.GraceInfo(Bytes, e.now),
		InodesGrace:    a.GraceInfo(Inodes, e.now),
	}
	if a.Dim == Tree {
		if ino, ok := e.treeRoot[index]; ok && ino != RootInode {
			v.MountPath = e.dirPath(ino)
		}
	}
	return v
}

// Accounts returns a snapshot of one dimension, sorted by ID.
func (e *Engine) Accounts(dim Dimension) []AccountView {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.viewsLocked(dim)
}

func (e *Engine) viewsLocked(dim Dimension) []AccountView {
	return e.viewsSorted(dim, true)
}

func (e *Engine) viewsSorted(dim Dimension, byID bool) []AccountView {
	list := make([]AccountView, 0, len(e.accts[dim]))
	for i, a := range e.accts[dim] {
		list = append(list, e.view(a, i))
	}
	if byID {
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	}
	return list
}

// AccountsView runs fn over every account of a dimension under the read
// lock, in account creation order. It avoids allocating a snapshot, which
// matters in hot verification loops. fn must not call back into the engine.
func (e *Engine) AccountsView(dim Dimension, fn func(AccountView)) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for i, a := range e.accts[dim] {
		fn(e.view(a, i))
	}
}

// AccountCount returns the number of accounts in a dimension.
func (e *Engine) AccountCount(dim Dimension) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.accts[dim])
}

// Report returns a dashboard snapshot. At most maxDenials refusals and
// maxActivity audit entries are returned, newest first.
func (e *Engine) Report(maxDenials, maxActivity int) Report {
	e.mu.RLock()
	defer e.mu.RUnlock()
	r := Report{
		Now:    e.now,
		Users:  e.viewsLocked(User),
		Groups: e.viewsLocked(Group),
		Trees:  e.viewsLocked(Tree),
		Stats: Stats{
			Nodes:          int64(len(e.live)),
			DenialsTotal:   e.denialsTotal,
			DenialsDropped: e.denialsLost,
			Activities:     e.activityTotal,
		},
	}
	r.Stats.Accounts = len(e.accts[User]) + len(e.accts[Group]) + len(e.accts[Tree])
	for _, n := range e.live {
		r.Stats.LiveBytes += n.size
	}
	r.Denials = e.denials.newest(maxDenials)
	r.Activity = e.activity.newest(maxActivity)
	return r
}

// Denials returns a copy of the refusal log, oldest first.
func (e *Engine) Denials() []Denial {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.denials.oldest(0)
}

// ActivityLog returns a copy of the administrative audit log, oldest first.
func (e *Engine) ActivityLog() []ActivityEntry {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.activity.oldest(0)
}

// Stats returns engine-wide counters.
func (e *Engine) Stats() Stats {
	return e.Report(0, 0).Stats
}

// DenialCount returns the number of refusals ever recorded, including those
// already dropped from the in-memory log.
func (e *Engine) DenialCount() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.denialsTotal
}
