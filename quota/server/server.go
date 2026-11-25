// Package server exposes the quota engine over HTTP with a dashboard and a
// small JSON API.
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"filequota/quota"
)

//go:embed dashboard.html
var assets embed.FS

// Server wraps an engine.
type Server struct {
	Eng      *quota.Engine
	Denials  int
	Activity int
}

// New creates a server.
func New(eng *quota.Engine) *Server {
	return &Server{Eng: eng, Denials: 200, Activity: 200}
}

// Handler returns the HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/report", s.handleReport)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/limits", s.handleLimits)
	mux.HandleFunc("/api/grace", s.handleGrace)
	mux.HandleFunc("/api/clock", s.handleClock)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	return mux
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	rep := s.Eng.Report(s.Denials, s.Activity)
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Eng.VerifyState())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type limitsRequest struct {
	Dim    string      `json:"dim"`
	ID     string      `json:"id"`
	Actor  string      `json:"actor"`
	Bytes  quota.Limit `json:"bytes"`
	Inodes quota.Limit `json:"inodes"`
}

func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var req limitsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	dim, ok := quota.ParseDimension(req.Dim)
	if !ok {
		writeErr(w, http.StatusBadRequest, "未知维度: "+req.Dim)
		return
	}
	if req.ID == "" || req.Actor == "" {
		writeErr(w, http.StatusBadRequest, "必须提供账户和操作人")
		return
	}
	if err := s.Eng.SetLimits(dim, req.ID, [2]quota.Limit{req.Bytes, req.Inodes}, req.Actor); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	view, _ := s.Eng.AccountView(dim, req.ID)
	writeJSON(w, http.StatusOK, view)
}

type graceRequest struct {
	Dim     string `json:"dim"`
	ID      string `json:"id"`
	Actor   string `json:"actor"`
	Seconds int64  `json:"seconds"`
}

func (s *Server) handleGrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var req graceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	dim, ok := quota.ParseDimension(req.Dim)
	if !ok {
		writeErr(w, http.StatusBadRequest, "未知维度: "+req.Dim)
		return
	}
	if req.Seconds <= 0 {
		writeErr(w, http.StatusBadRequest, "延长秒数必须为正")
		return
	}
	if err := s.Eng.GrantGrace(dim, req.ID, time.Duration(req.Seconds)*time.Second, req.Actor); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	view, _ := s.Eng.AccountView(dim, req.ID)
	writeJSON(w, http.StatusOK, view)
}

// handleClock advances or reads the simulated clock. Advancing the clock is
// how an operator can watch grace periods run out without waiting.
func (s *Server) handleClock(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"now": s.Eng.Now().Format(time.RFC3339Nano)})
	case http.MethodPost:
		q := r.URL.Query()
		secs, err := strconv.ParseInt(q.Get("advance_seconds"), 10, 64)
		if err != nil || secs <= 0 {
			writeErr(w, http.StatusBadRequest, "需要正的 advance_seconds")
			return
		}
		s.Eng.Advance(time.Duration(secs) * time.Second)
		writeJSON(w, http.StatusOK, map[string]string{"now": s.Eng.Now().Format(time.RFC3339Nano)})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET/POST")
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	rep := s.Eng.Report(s.Denials, s.Activity)
	data := templateData{
		Report: rep,
		Mounts: mountList(rep),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type templateData struct {
	Report quota.Report
	Mounts []string
}

func mountList(rep quota.Report) []string {
	var out []string
	for _, t := range rep.Trees {
		out = append(out, t.ID)
	}
	return out
}

var templateFuncs = template.FuncMap{
	"bytes": humanBytes,
	"pct": func(used, limit int64) float64 {
		if limit <= 0 {
			return 0
		}
		return float64(used) / float64(limit) * 100
	},
	"dur": humanDuration,
	"t": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format("2006-01-02 15:04:05")
	},
	"kind": func(k quota.Kind) string { return k.Label() },
	"dim":  func(d quota.Dimension) string { return d.Label() },
	"lim": func(v int64) string {
		if v <= 0 {
			return "不限"
		}
		return humanBytes(v)
	},
	"limInode": func(v int64) string {
		if v <= 0 {
			return "不限"
		}
		return strconv.FormatInt(v, 10)
	},
	"worstClass": func(v quota.AccountView) string {
		if c := rowClassOf(v.BytesUsed, v.BytesLimitNow, v.BytesGrace); c == "over-hard" {
			return c
		} else if c == "grace-expired" {
			return c
		} else if c := rowClassOf(v.InodesUsed, v.InodesLimitNow, v.InodesGrace); c != "" {
			return c
		}
		return rowClassOf(v.BytesUsed, v.BytesLimitNow, v.BytesGrace)
	},
	"barClass": func(used, limit int64, grace quota.GraceView) string {
		switch {
		case grace.Expired || (limit > 0 && used >= limit):
			return "danger"
		case grace.Active:
			return "warn"
		case limit > 0 && float64(used)/float64(limit) >= 0.8:
			return "warn"
		}
		return ""
	},
	"pctText": func(used, limit int64) string {
		if limit <= 0 {
			return "无硬限额"
		}
		return fmt.Sprintf("%.1f%%", float64(used)/float64(limit)*100)
	},
	"dict": func(kv ...any) map[string]any {
		m := make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
	"join": strings.Join,
	"num": func(v int64) string {
		// Group thousands so multi-gigabyte byte counts stay readable.
		s := strconv.FormatInt(v, 10)
		neg := strings.HasPrefix(s, "-")
		if neg {
			s = s[1:]
		}
		var b strings.Builder
		for i, c := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(c)
		}
		if neg {
			return "-" + b.String()
		}
		return b.String()
	},
}

var dashboardTmpl = template.Must(template.New("dashboard").Funcs(templateFuncs).ParseFS(assets, "dashboard.html"))

func rowClassOf(used, limit int64, grace quota.GraceView) string {
	switch {
	case limit > 0 && used > limit:
		return "over-hard"
	case grace.Expired:
		return "grace-expired"
	case grace.Active:
		return "grace"
	}
	return ""
}

// humanBytes formats a byte count.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < len(units)-1 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %s", float64(n)/float64(div), units[exp])
}

func humanDuration(seconds float64) string {
	if seconds <= 0 {
		return "—"
	}
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
