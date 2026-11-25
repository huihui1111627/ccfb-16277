// Package quota implements an in-memory filesystem quota engine.
//
// A live inode is charged simultaneously against three independent
// dimensions: its owner user, its owning group, and the directory tree it
// belongs to. Each account has a soft and a hard limit for both bytes and
// inode count. Usage above the soft limit is tolerated while a grace period
// runs; once the grace period expires and the account is still above the soft
// limit, the soft limit becomes the enforced ceiling. Usage above the hard
// limit is refused outright.
//
// File system operations are simulated in memory; no real files are touched.
package quota

import (
	"encoding/json"
	"fmt"
	"time"
)

// DefaultGrace is used for accounts that do not configure a grace period.
const DefaultGrace = 24 * time.Hour

// Dimension identifies one quota axis.
type Dimension int

const (
	User Dimension = iota
	Group
	Tree
)

// AllDimensions is the canonical iteration order.
var AllDimensions = [...]Dimension{User, Group, Tree}

// Dimensions is a slice form of AllDimensions.
var Dimensions = AllDimensions[:]

// String returns the stable identifier used on the wire.
func (d Dimension) String() string {
	switch d {
	case User:
		return "user"
	case Group:
		return "group"
	case Tree:
		return "tree"
	}
	return "unknown"
}

// Label returns the Chinese display name.
func (d Dimension) Label() string {
	switch d {
	case User:
		return "用户"
	case Group:
		return "组"
	case Tree:
		return "目录树"
	}
	return "未知"
}

// MarshalJSON encodes the dimension as its wire identifier.
func (d Dimension) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON parses the wire identifier.
func (d *Dimension) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, ok := ParseDimension(s)
	if !ok {
		return fmt.Errorf("unknown dimension %q", s)
	}
	*d = parsed
	return nil
}

// ParseDimension parses the wire identifier.
func ParseDimension(s string) (Dimension, bool) {
	for _, d := range AllDimensions {
		if d.String() == s {
			return d, true
		}
	}
	return 0, false
}

// Kind identifies a limitable resource.
type Kind int

const (
	Bytes Kind = iota
	Inodes
)

// AllKinds is the canonical iteration order.
var AllKinds = [...]Kind{Bytes, Inodes}

// String returns the stable identifier used on the wire.
func (k Kind) String() string {
	switch k {
	case Bytes:
		return "bytes"
	case Inodes:
		return "inodes"
	}
	return "unknown"
}

// Label returns the Chinese display name.
func (k Kind) Label() string {
	switch k {
	case Bytes:
		return "空间"
	case Inodes:
		return "文件数"
	}
	return "未知"
}

// MarshalJSON encodes the kind as its wire identifier.
func (k Kind) MarshalJSON() ([]byte, error) { return json.Marshal(k.String()) }

// UnmarshalJSON parses the wire identifier.
func (k *Kind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, ok := ParseKind(s)
	if !ok {
		return fmt.Errorf("unknown kind %q", s)
	}
	*k = parsed
	return nil
}

// ParseKind parses the wire identifier.
func ParseKind(s string) (Kind, bool) {
	for _, k := range AllKinds {
		if k.String() == s {
			return k, true
		}
	}
	return 0, false
}

// Limit is a soft/hard limit pair. Zero means unlimited.
type Limit struct {
	Soft int64 `json:"soft"`
	Hard int64 `json:"hard"`
}

// NodeID is an inode handle.
type NodeID uint64

// NodeKind is the inode type.
type NodeKind uint8

const (
	File NodeKind = iota
	Dir
	Symlink
)

// String returns the stable identifier.
func (k NodeKind) String() string {
	switch k {
	case Dir:
		return "dir"
	case Symlink:
		return "symlink"
	}
	return "file"
}

// GraceView is the grace-period status for one resource.
type GraceView struct {
	// Active means a grace clock is running (it may already have expired).
	Active bool `json:"active"`
	// Expired means the deadline passed while usage stayed above soft.
	Expired bool `json:"expired"`
	// Remaining is seconds until the deadline; 0 when expired or absent.
	Remaining float64 `json:"remaining_seconds"`
	// Deadline is the absolute end of the grace period (zero when absent).
	Deadline time.Time `json:"deadline,omitzero"`
}

// Account is the live quota state of one user, group or directory tree.
type Account struct {
	ID     string
	Dim    Dimension
	Used   [2]int64
	Limits [2]Limit
	Grace  time.Duration

	grace [2]graceState
}

type graceState struct {
	active   bool
	deadline time.Time
}

// SoftEffective returns the soft limit that can actually start a grace
// period. A soft limit at or above a hard limit never takes effect (the hard
// limit refuses first) and is reported as absent (0). A soft limit with no
// hard limit is meaningful: usage may grow past it, and once the grace period
// runs out the soft limit itself becomes the enforced ceiling.
func (a *Account) SoftEffective(k Kind) int64 {
	soft, hard := a.Limits[k].Soft, a.Limits[k].Hard
	if soft <= 0 {
		return 0
	}
	if hard > 0 && soft >= hard {
		return 0
	}
	return soft
}

// EffectiveLimit returns the ceiling enforced right now. If a grace period has
// expired while usage is still above the soft limit, the soft limit is
// enforced instead of the hard limit.
func (a *Account) EffectiveLimit(k Kind, now time.Time) int64 {
	hard := a.Limits[k].Hard
	soft := a.SoftEffective(k)
	g := a.grace[k]
	if soft > 0 && g.active && !now.Before(g.deadline) && a.Used[k] > soft {
		return soft
	}
	return hard
}

// GraceInfo reports the grace status of a resource.
func (a *Account) GraceInfo(k Kind, now time.Time) GraceView {
	g := a.grace[k]
	if !g.active {
		return GraceView{}
	}
	soft := a.SoftEffective(k)
	if now.Before(g.deadline) {
		return GraceView{Active: true, Remaining: g.deadline.Sub(now).Seconds(), Deadline: g.deadline}
	}
	if soft > 0 && a.Used[k] > soft {
		return GraceView{Active: true, Expired: true, Deadline: g.deadline}
	}
	return GraceView{}
}

func (a *Account) graceActive(k Kind) bool { return a.grace[k].active }

// updateGrace arms or clears grace clocks after usage or limits changed.
func (a *Account) updateGrace(now time.Time) {
	for k := Bytes; k <= Inodes; k++ {
		soft := a.SoftEffective(k)
		if soft > 0 && a.Used[k] > soft {
			if !a.grace[k].active {
				d := a.Grace
				if d <= 0 {
					d = DefaultGrace
				}
				a.grace[k] = graceState{active: true, deadline: now.Add(d)}
			}
			continue
		}
		a.grace[k] = graceState{}
	}
}

// extendGrace pushes an expired or missing grace deadline to max(now,
// deadline)+ext. It reports whether a clock was running.
func (a *Account) extendGrace(k Kind, now time.Time, ext time.Duration) bool {
	if !a.grace[k].active {
		return false
	}
	base := a.grace[k].deadline
	if base.Before(now) {
		base = now
	}
	a.grace[k].deadline = base.Add(ext)
	return true
}

// Op identifies a filesystem or administrative operation.
type Op string

const (
	OpCreate     Op = "create"
	OpMkdir      Op = "mkdir"
	OpSymlink    Op = "symlink"
	OpWrite      Op = "write"
	OpTruncate   Op = "truncate"
	OpDelete     Op = "delete"
	OpMove       Op = "move"
	OpHardlink   Op = "hardlink"
	OpChownUser  Op = "chown_user"
	OpChownGroup Op = "chown_group"
	OpSetLimits  Op = "set_limits"
	OpGrantGrace Op = "grant_grace"
)

// Label returns the Chinese display name.
func (o Op) Label() string {
	switch o {
	case OpCreate:
		return "新建文件"
	case OpMkdir:
		return "新建目录"
	case OpSymlink:
		return "新建符号链接"
	case OpWrite:
		return "写入"
	case OpTruncate:
		return "截断"
	case OpDelete:
		return "删除"
	case OpMove:
		return "移动"
	case OpHardlink:
		return "硬链接"
	case OpChownUser:
		return "改属主"
	case OpChownGroup:
		return "改属组"
	case OpSetLimits:
		return "修改限额"
	case OpGrantGrace:
		return "延长宽限期"
	}
	return string(o)
}

// Denial is the audit record of an operation refused by a quota.
type Denial struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Op   Op        `json:"op"`
	Ino  NodeID    `json:"ino"`
	Path string    `json:"path,omitempty"`
	Dim  Dimension `json:"dim"`
	// Account is the user, group or tree that refused.
	Account string `json:"account"`
	Kind    Kind   `json:"kind"`
	// Used is the account usage on which the request was evaluated. For a
	// clipped write this is the usage before the operation started.
	Used int64 `json:"used"`
	// Demand is the amount of the resource this operation wanted to add.
	Demand int64 `json:"demand"`
	// Shortfall is Used + Demand - Limit, always positive.
	Shortfall int64 `json:"shortfall"`
	// Requested and Written describe the whole operation; they differ for a
	// clipped write or grow-truncate.
	Requested int64 `json:"requested"`
	Written   int64 `json:"written"`
	// Limit is the ceiling in force at refusal time (hard, or soft when the
	// grace period had expired).
	Limit int64 `json:"limit"`
	// SoftAt and HardAt snapshot the configured limits at refusal time.
	SoftAt int64 `json:"soft_at"`
	HardAt int64 `json:"hard_at"`
	// GraceExpired reports that the refusal enforced the soft limit because
	// the grace period had run out.
	GraceExpired bool `json:"grace_expired"`
}

// Reason explains the refusal in Chinese.
func (d Denial) Reason() string {
	if d.GraceExpired {
		return "超过硬限额（宽限期已过，按软限额执行）"
	}
	return "超过硬限额"
}

// Fits reports whether the denial arithmetic is self-consistent:
// the request did not fit under the ceiling in force.
func (d Denial) Fits() bool {
	return d.Limit > 0 && d.Demand > 0 && d.Used+d.Demand > d.Limit
}

// Error renders the denial.
func (d Denial) Error() string {
	path := d.Path
	if path == "" {
		path = fmt.Sprintf("inode %d", d.Ino)
	}
	return fmt.Sprintf("%s 被拒绝（%s）: %s %q 的%s限额为 %d，已用 %d，请求新增 %d，实际写入 %d（%s）",
		d.Op, path, d.Dim, d.Account, d.Kind, d.Limit, d.Used, d.Demand, d.Written, d.Reason())
}

// QuotaError is returned when a quota refusal happened. For a partially
// completed write or grow-truncate, Written is smaller than Requested and the
// operation's Result still reports what was actually written.
type QuotaError struct {
	Denial Denial
}

// Error implements error.
func (e *QuotaError) Error() string { return e.Denial.Error() }

// OpError is a non-quota refusal: bad name, missing entry, invalid move.
type OpError struct {
	Op     Op
	Reason string
	Ino    NodeID
}

// Error implements error.
func (e *OpError) Error() string { return fmt.Sprintf("%s 失败: %s", e.Op, e.Reason) }

// Result describes the effect of a mutating operation.
type Result struct {
	Ino     NodeID `json:"ino"`
	Size    int64  `json:"size"`
	Written int64  `json:"written"`
	Freed   int64  `json:"freed"`
}

// ActivityEntry audits an administrative limits or grace change.
type ActivityEntry struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Action  Op        `json:"action"`
	Dim     Dimension `json:"dim"`
	Account string    `json:"account"`
	Changes []string  `json:"changes"`
}

// AccountView is a read-only snapshot of one account.
type AccountView struct {
	ID             string    `json:"id"`
	Dim            Dimension `json:"dim"`
	BytesUsed      int64     `json:"bytes_used"`
	InodesUsed     int64     `json:"inodes_used"`
	BytesSoft      int64     `json:"bytes_soft"`
	BytesHard      int64     `json:"bytes_hard"`
	InodesSoft     int64     `json:"inodes_soft"`
	InodesHard     int64     `json:"inodes_hard"`
	BytesLimitNow  int64     `json:"bytes_limit_now"`
	InodesLimitNow int64     `json:"inodes_limit_now"`
	BytesGrace     GraceView `json:"bytes_grace"`
	InodesGrace    GraceView `json:"inodes_grace"`
	// MountPath is the directory that roots this tree; empty for users and
	// groups, and for trees that are not currently mounted.
	MountPath string `json:"mount_path,omitempty"`
}

// Stats are engine-wide counters.
type Stats struct {
	Accounts       int   `json:"accounts"`
	Nodes          int64 `json:"nodes"`
	LiveBytes      int64 `json:"live_bytes"`
	DenialsTotal   int64 `json:"denials_total"`
	DenialsDropped int64 `json:"denials_dropped"`
	Activities     int64 `json:"activities_total"`
}

// Report is a full state snapshot for the dashboard.
type Report struct {
	Now      time.Time       `json:"now"`
	Users    []AccountView   `json:"users"`
	Groups   []AccountView   `json:"groups"`
	Trees    []AccountView   `json:"trees"`
	Denials  []Denial        `json:"denials"`
	Activity []ActivityEntry `json:"activity"`
	Stats    Stats           `json:"stats"`
}

// VerifyReport is the result of an independent full scan.
type VerifyReport struct {
	OK       bool      `json:"ok"`
	At       time.Time `json:"at"`
	Problems []string  `json:"problems"`
	Nodes    int64     `json:"nodes"`
	Bytes    int64     `json:"bytes"`
	Accounts int       `json:"accounts_checked"`
	Denials  int       `json:"denials_checked"`
}
