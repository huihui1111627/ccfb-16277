package main

import (
	"fmt"
	"time"

	"filequota/quota"
)

type officeOptions struct {
	users, groups, townships int
	grace                    time.Duration
	start                    time.Time
}

// officeSpecs is the bootstrap layout plus the account tables it created.
type officeSpecs struct {
	eng     *quota.Engine
	users   []string
	groups  []string
	towns   []townDir
	byTribe map[string]int
}

type townDir struct {
	id   string // tree account ID
	name string
	ino  quota.NodeID
	work quota.NodeID
}

var townshipNames = []string{
	"城关镇", "东关镇", "西河镇", "南坪乡", "北岭乡", "青石镇", "柳林镇", "沙河镇",
	"白水乡", "黄岗镇", "石桥镇", "杨树乡", "马场镇", "龙泉乡", "双塔镇", "三元乡",
	"永安镇", "太平乡", "兴隆镇", "红旗乡", "建设镇", "幸福乡", "光明镇", "长风乡",
}

var groupNames = []string{"编纂一组", "编纂二组", "编纂三组", "年鉴组", "资料组", "数字化组", "审校组", "印刷联络组"}

// buildOffice creates the directory skeleton and the account table. Sizes are
// deliberately small so the dashboard shows real pressure quickly, mirroring
// the "one group filled the server" incident.
func buildOffice(o officeOptions) (*quota.Engine, officeSpecs, error) {
	if o.users <= 0 || o.groups <= 0 || o.townships <= 0 {
		return nil, officeSpecs{}, fmt.Errorf("用户、组和乡镇数都必须为正")
	}
	users := make([]quota.AccountSpec, 0, o.users)
	for i := 0; i < o.users; i++ {
		// Editors get tight soft limits, with room to grow before the hard
		// ceiling; one in twelve is a reviewer with almost no quota.
		prof := i % 12
		spec := quota.AccountSpec{
			ID:     fmt.Sprintf("编辑%02d", i+1),
			Bytes:  quota.Limit{Soft: 2 << 30, Hard: 6 << 30},
			Inodes: quota.Limit{Soft: 2000, Hard: 5000},
			Grace:  o.grace,
		}
		if prof == 0 {
			spec.Bytes = quota.Limit{Soft: 200 << 20, Hard: 1 << 30}
			spec.Inodes = quota.Limit{Soft: 200, Hard: 600}
		}
		if prof == 5 {
			spec.Bytes = quota.Limit{Soft: 4 << 30, Hard: 12 << 30}
			spec.Inodes = quota.Limit{Soft: 4000, Hard: 10000}
		}
		users = append(users, spec)
	}
	groups := make([]quota.AccountSpec, 0, o.groups)
	for i := 0; i < o.groups; i++ {
		name := fmt.Sprintf("组%02d", i+1)
		if i < len(groupNames) {
			name = groupNames[i]
		}
		// Every group shares one township's worth of default quota; the
		// oversized scans must be stopped by the tree limit as well.
		groups = append(groups, quota.AccountSpec{
			ID:     name,
			Bytes:  quota.Limit{Soft: 12 << 30, Hard: 40 << 30},
			Inodes: quota.Limit{Soft: 12000, Hard: 40000},
			Grace:  o.grace,
		})
	}
	trees := make([]quota.AccountSpec, 0, o.townships)
	for i := 0; i < o.townships; i++ {
		trees = append(trees, quota.AccountSpec{
			ID:     treeID(i),
			Bytes:  quota.Limit{Soft: 3 << 30, Hard: 8 << 30},
			Inodes: quota.Limit{Soft: 3000, Hard: 8000},
			Grace:  o.grace,
		})
	}

	eng, err := quota.New(quota.Config{Users: users, Groups: groups, Trees: trees}, o.start, o.grace)
	if err != nil {
		return nil, officeSpecs{}, err
	}
	specs := officeSpecs{eng: eng, byTribe: map[string]int{}}
	for _, u := range users {
		specs.users = append(specs.users, u.ID)
	}
	for _, g := range groups {
		specs.groups = append(specs.groups, g.ID)
	}
	// Skeleton directories belong to an unlimited service account so the
	// layout itself never trips a real quota.
	for i := 0; i < o.townships; i++ {
		name := fmt.Sprintf("乡镇%02d", i+1)
		if i < len(townshipNames) {
			name = townshipNames[i]
		}
		id := treeID(i)
		m, err := eng.Mkdir("系统", "系统", quota.RootInode, name, id)
		if err != nil {
			return nil, officeSpecs{}, fmt.Errorf("创建乡镇目录 %s: %w", name, err)
		}
		work, err := eng.Mkdir("系统", "系统", m.Ino, "扫描件", "")
		if err != nil {
			return nil, officeSpecs{}, fmt.Errorf("创建 %s/扫描件: %w", name, err)
		}
		specs.towns = append(specs.towns, townDir{id: id, name: name, ino: m.Ino, work: work.Ino})
	}
	return eng, specs, nil
}

func treeID(i int) string { return fmt.Sprintf("tree-%02d", i+1) }

// loadSampleFiles creates a few representative files so the dashboard is not
// empty: scanned volumes per township, with the first township deliberately
// pushed above its soft limit so it shows a running grace countdown and a
// refusal, mirroring the incident this tool was written for.
func loadSampleFiles(eng *quota.Engine, specs officeSpecs, start time.Time) {
	eng.SetNow(start)
	// 城关镇 takes the bulk of a large digitisation batch: nine 512 MiB
	// volumes against a 3 GiB soft tree limit. That starts a grace period.
	big := specs.towns[0]
	// Pick an editor with the ordinary quota profile, so the pressure shown
	// is the tree and group limit, not the low-quota reviewer profile.
	bigOwner := specs.users[1]
	if len(specs.users) > 13 {
		bigOwner = specs.users[13]
	}
	bigGroup := specs.groups[0]
	write := func(i int, size int64) bool {
		name := fmt.Sprintf("高清卷%02d.tif", i+1)
		res, err := eng.Create(bigOwner, bigGroup, big.work, name)
		if err != nil {
			eng.Advance(time.Minute)
			return false
		}
		_, err = eng.Write(res.Ino, size)
		eng.Advance(time.Minute)
		return err == nil
	}
	for i := 0; i < 9; i++ {
		write(i, 512<<20)
	}
	// An administrator grants more time while the editorial group promises
	// to trim the batch.
	_ = eng.GrantGrace(quota.Tree, big.id, 12*time.Hour, "李主任")
	eng.Advance(6 * time.Hour)
	// The group keeps uploading; this batch runs into the editor's 6 GiB
	// hard limit, so it is partly refused.
	for i := 9; i < 14; i++ {
		write(i, 512<<20)
	}
	// The remaining townships get a handful of normal-sized scans.
	for ti := 1; ti < len(specs.towns); ti++ {
		town := specs.towns[ti]
		owner := specs.users[(ti*5)%len(specs.users)]
		group := specs.groups[ti%len(specs.groups)]
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("卷%02d-%02d.tif", ti+1, i+1)
			res, err := eng.Create(owner, group, town.work, name)
			if err != nil {
				eng.Advance(3 * time.Minute)
				continue
			}
			size := int64(40+(ti*7+i*11)%120) << 20
			if _, err := eng.Write(res.Ino, size); err != nil {
				eng.Advance(3 * time.Minute)
				continue
			}
			eng.Advance(3 * time.Minute)
		}
	}
	// A couple of renames so the trees have archives as well as scans.
	for ti := 0; ti < len(specs.towns) && ti < 4; ti++ {
		town := specs.towns[ti]
		ents := eng.Dirents(town.work)
		if len(ents) == 0 {
			continue
		}
		_, _ = eng.Move(town.work, ents[0].Name, town.ino, "归档-"+ents[0].Name)
	}
}
