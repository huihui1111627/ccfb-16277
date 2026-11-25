package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filequota/quota"
)

func fixture(t *testing.T) *Server {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	eng, err := quota.New(quota.Config{
		Users:  []quota.AccountSpec{{ID: "u1", Bytes: quota.Limit{Soft: 1000, Hard: 2000}, Inodes: quota.Limit{Soft: 5, Hard: 10}}},
		Groups: []quota.AccountSpec{{ID: "g1", Bytes: quota.Limit{Soft: 5000, Hard: 10000}}},
		Trees:  []quota.AccountSpec{{ID: "tree-01", Bytes: quota.Limit{Soft: 5000, Hard: 10000}}},
	}, start, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := eng.Mkdir("u1", "g1", quota.RootInode, "城关镇", "tree-01")
	if err != nil {
		t.Fatal(err)
	}
	f, err := eng.Create("u1", "g1", dir.Ino, "卷01.tif")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Write(f.Ino, 900); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Write(f.Ino, 900); err != nil { // crosses soft, within hard
		t.Fatal(err)
	}
	return New(eng)
}

func TestDashboardRenders(t *testing.T) {
	s := fixture(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"文件服务器配额面板", "城关镇", "u1", "g1", "tree-01",
		"空间软/硬限额", "文件数", "宽限期", "被拒绝的操作",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// The soft limit is exceeded, so a grace countdown must be visible.
	if !strings.Contains(body, "剩余") {
		t.Error("dashboard should show a running grace period")
	}
}

func TestDashboardEscapesNames(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	eng, err := quota.New(quota.Config{Users: []quota.AccountSpec{{ID: `<script>alert(1)</script>`}}}, start, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := New(eng)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("account ID must be HTML-escaped")
	}
}

func TestReportJSON(t *testing.T) {
	s := fixture(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/report", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var rep quota.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Users) != 1 || len(rep.Trees) != 2 { // root tree is implicit
		t.Fatalf("unexpected report: %+v", rep)
	}
	if rep.Users[0].BytesUsed != 1800 {
		t.Fatalf("usage: %d", rep.Users[0].BytesUsed)
	}
	if !rep.Users[0].BytesGrace.Active {
		t.Fatal("soft limit crossed, grace should be active")
	}
	// Refusal log must be populated once something is written past the hard
	// limit.
	f := rep.Users[0]
	_ = f
	dirs := s.Eng.Dirents(quota.RootInode)
	if len(dirs) == 0 {
		t.Fatal("fixture directory missing")
	}
	files := s.Eng.Dirents(dirs[0].Ino)
	if _, err := s.Eng.Write(files[0].Ino, 1000); err == nil {
		t.Fatal("expected refusal")
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/report", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Denials) != 1 {
		t.Fatalf("want one denial, got %+v", rep.Denials)
	}
	if rep.Denials[0].Op != quota.OpWrite || rep.Denials[0].Written != 200 {
		t.Fatalf("unexpected denial: %+v", rep.Denials[0])
	}
}

func TestSetLimitsAPI(t *testing.T) {
	s := fixture(t)
	body := `{"dim":"user","id":"u1","actor":"办主任","bytes":{"soft":5000,"hard":20000},"inodes":{"soft":50,"hard":100}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/limits", strings.NewReader(body))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var view quota.AccountView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.BytesHard != 20000 {
		t.Fatalf("limits not applied: %+v", view)
	}
	acts := s.Eng.ActivityLog()
	if len(acts) != 1 || acts[0].Actor != "办主任" {
		t.Fatalf("audit: %+v", acts)
	}
}

func TestSetLimitsRejectsBelowUsage(t *testing.T) {
	s := fixture(t)
	body := `{"dim":"user","id":"u1","actor":"admin","bytes":{"hard":100}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/limits", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGrantGraceAPI(t *testing.T) {
	s := fixture(t)
	body := `{"dim":"user","id":"u1","actor":"主任","seconds":7200}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/grace", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var view quota.AccountView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.BytesGrace.Active || view.BytesGrace.Expired {
		t.Fatalf("grace should be extended and running: %+v", view.BytesGrace)
	}
	if view.BytesGrace.Remaining < 7000 {
		t.Fatalf("grace should be at least the extension: %+v", view.BytesGrace)
	}
}

func TestGraceAPIRejectsAbsentPeriod(t *testing.T) {
	s := fixture(t)
	body := `{"dim":"group","id":"g1","actor":"主任","seconds":60}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/grace", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestClockEndpoint(t *testing.T) {
	s := fixture(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/clock?advance_seconds=7200", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	v, _ := s.Eng.AccountView(quota.User, "u1")
	if !v.BytesGrace.Expired {
		t.Fatalf("after advancing past the grace period the soft limit must be enforced: %+v", v.BytesGrace)
	}
	if v.BytesLimitNow != 1000 {
		t.Fatalf("enforced limit should now be the soft limit: %d", v.BytesLimitNow)
	}
}

func TestVerifyEndpoint(t *testing.T) {
	s := fixture(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/verify", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var rep quota.VerifyReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("verify failed: %v", rep.Problems)
	}
}

func TestDashboardShowsActivityAndMountPath(t *testing.T) {
	s := fixture(t)
	if err := s.Eng.SetLimits(quota.User, "u1", [2]quota.Limit{{Soft: 5000, Hard: 20000}, {}}, "办主任"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{"办主任", "空间软限额", "挂载目录", "/城关镇"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

func TestUnknownPath(t *testing.T) {
	s := fixture(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}
