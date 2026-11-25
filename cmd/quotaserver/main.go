// Command quotaserver runs the in-memory quota simulator and serves the usage
// dashboard. It seeds a realistic layout for a local gazetteer office: one
// directory tree per township editorial group, shared user and group accounts,
// and per-township limits that reproduce the "one group uploads terabytes"
// problem this tool exists to demonstrate.
//
// Typical use:
//
//	quotaserver -addr :8080
//	quotaserver -simulate -steps 2000000
//	quotaserver -simulate -steps 200000 -seed 7 -verify
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"filequota/quota/server"
	"filequota/quota/sim"
)

func main() {
	var (
		addr       = flag.String("addr", ":8080", "监听地址")
		users      = flag.Int("users", 48, "用户数")
		groups     = flag.Int("groups", 8, "组数")
		townships  = flag.Int("townships", 24, "乡镇（目录树）数")
		graceHours = flag.Float64("grace-hours", 24, "默认宽限期（小时）")
		startISO   = flag.String("start", "2026-01-01T00:00:00Z", "模拟起始时间 (RFC3339)")
		simulate   = flag.Bool("simulate", false, "只运行随机模拟并输出报告，不起服务")
		steps      = flag.Int("steps", 200_000, "模拟步数")
		seed       = flag.Int64("seed", 20260101, "随机种子")
		checkEvery = flag.Int("check-every", 5000, "每多少步做一次结构全量校验")
		maxLive    = flag.Int("max-live", 400, "模拟中最多存活 inode 数")
		verifyOnly = flag.Bool("verify", false, "模拟结束后额外打印最终校验结果")
		jsonOut    = flag.Bool("json", false, "模拟结果以 JSON 输出")
		maxWall    = flag.Duration("max-wall", 0, "模拟最长时间，0 表示不限")
		limitScale = flag.Float64("limit-scale", 1, "模拟时把全部限额乘以该系数（小规模演示用）")
	)
	flag.Parse()

	start, err := time.Parse(time.RFC3339, *startISO)
	if err != nil {
		log.Fatalf("起始时间格式错误: %v", err)
	}
	grace := time.Duration(*graceHours * float64(time.Hour))

	if *simulate {
		os.Exit(runSimulation(simOptions{
			seed: *seed, users: *users, groups: *groups, townships: *townships,
			grace: grace, start: start, steps: *steps, checkEvery: *checkEvery,
			maxLive: *maxLive, maxWall: *maxWall, json: *jsonOut, verify: *verifyOnly,
			limitScale: *limitScale,
		}))
	}

	eng, specs, err := buildOffice(officeOptions{
		users: *users, groups: *groups, townships: *townships, grace: grace, start: start,
	})
	if err != nil {
		log.Fatalf("初始化: %v", err)
	}
	loadSampleFiles(eng, specs, start)

	srv := server.New(eng)
	log.Printf("配额面板: http://localhost%s/ （%d 用户, %d 组, %d 乡镇目录树）",
		displayAddr(*addr), *users, *groups, *townships)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func displayAddr(addr string) string {
	if addr != "" && addr[0] == ':' {
		return addr
	}
	return addr
}

type simOptions struct {
	seed                       int64
	users, groups, townships   int
	grace                      time.Duration
	start                      time.Time
	steps, checkEvery, maxLive int
	maxWall                    time.Duration
	limitScale                 float64
	json, verify               bool
}

func runSimulation(o simOptions) int {
	runner, err := sim.NewRunner(sim.Options{
		Seed: o.seed, Users: o.users, Groups: o.groups, Trees: o.townships,
		Steps: o.steps, MaxLive: o.maxLive, Start: o.start,
		DefaultGrace: o.grace, CheckEvery: o.checkEvery, MaxWall: o.maxWall,
		LimitScale: o.limitScale,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败: %v\n", err)
		return 1
	}
	out := runner.Run()
	if o.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		printOutcome(out, o.verify)
	}
	if !out.OK {
		return 1
	}
	return 0
}

func printOutcome(out sim.Outcome, verify bool) {
	ops := make([]string, 0, len(out.Operations))
	for k := range out.Operations {
		ops = append(ops, k)
	}
	sort.Strings(ops)

	fmt.Printf("种子 %d，步数 %d，用时 %.1fs\n", out.Seed, out.Steps, out.ElapsedSec)
	fmt.Printf("拒绝 %d 次，写入被截断 %d 次，写入字节 %d\n", out.Denials, out.Clipped, out.Written)
	fmt.Printf("结构全量校验 %d 次\n", out.FullChecks)
	fmt.Println("各操作次数:")
	for _, k := range ops {
		fmt.Printf("  %-12s %d\n", k, out.Operations[k])
	}
	fmt.Printf("最终扫描校验: %s，%d 个 inode，%d 个账户\n",
		pass(out.FinalScan.OK), out.FinalScan.Nodes, out.FinalScan.Accounts)
	fmt.Printf("最终结构校验: %s\n", pass(out.Final.OK))
	if !out.FinalScan.OK {
		for _, p := range out.FinalScan.Problems {
			fmt.Println("  " + p)
		}
	}
	if !out.Final.OK {
		for _, p := range out.Final.Problems {
			fmt.Println("  " + p)
		}
	}
	if out.OK {
		fmt.Println("结果: 全部不变量成立")
	} else {
		fmt.Printf("结果: 发现 %d 个问题\n", len(out.Failures))
		for _, f := range out.Failures {
			fmt.Println("  " + f)
		}
	}
}

func pass(ok bool) string {
	if ok {
		return "通过"
	}
	return "失败"
}
