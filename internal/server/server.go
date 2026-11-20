// Package server exposes the profiling service over HTTP and serves the
// flame graph UI.
package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"aquaprofile/internal/diff"
	"aquaprofile/internal/merge"
	"aquaprofile/internal/store"
)

//go:embed web
var webFS embed.FS

// Server wires the store to the HTTP routes.
type Server struct {
	store *store.Store
	mux   *http.ServeMux
}

// New builds the HTTP handler.
func New(st *store.Store) http.Handler {
	s := &Server{store: st, mux: http.NewServeMux()}
	s.routes()
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/modules", s.handleModules)
	s.mux.HandleFunc("POST /api/samples", s.handleSamples)
	s.mux.HandleFunc("GET /api/programs", s.handlePrograms)
	s.mux.HandleFunc("GET /api/flame", s.handleFlame)
	s.mux.HandleFunc("GET /api/hot", s.handleHot)
	s.mux.HandleFunc("GET /api/diff", s.handleDiff)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)

	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("GET /", http.FileServer(http.FS(webRoot)))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) handleModules(w http.ResponseWriter, r *http.Request) {
	var u store.ModuleUpload
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.PutModules(u); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	var b store.Batch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.PutBatch(b); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "accepted": len(b.Samples)})
}

func (s *Server) handlePrograms(w http.ResponseWriter, r *http.Request) {
	type prog struct {
		Name     string   `json:"name"`
		Versions []string `json:"versions"`
	}
	out := make([]prog, 0)
	for _, p := range s.store.Programs() {
		out = append(out, prog{Name: p, Versions: s.store.Versions(p)})
	}
	writeJSON(w, map[string]any{"programs": out})
}

// parseFilter reads time bounds and label.* query parameters.
func parseFilter(r *http.Request) store.Filter {
	q := r.URL.Query()
	f := store.Filter{Labels: map[string]string{}}
	if v := q.Get("start_ns"); v != "" {
		f.StartNs, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := q.Get("end_ns"); v != "" {
		f.EndNs, _ = strconv.ParseInt(v, 10, 64)
	}
	for key, vals := range q {
		if strings.HasPrefix(key, "label.") && len(vals) > 0 {
			f.Labels[strings.TrimPrefix(key, "label.")] = vals[0]
		}
	}
	if len(f.Labels) == 0 {
		f.Labels = nil
	}
	return f
}

type nodeDTO struct {
	Module    string     `json:"module"`
	Func      string     `json:"func"`
	Self      int64      `json:"self_ns"`
	Total     int64      `json:"total_ns"`
	Count     int64      `json:"count"`
	SelfCount int64      `json:"self_count"`
	Children  []*nodeDTO `json:"children,omitempty"`
}

type holeDTO struct {
	Module  string `json:"module"`
	Func    string `json:"func"`
	Kind    string `json:"kind"`
	Samples int64  `json:"samples"`
	Weight  int64  `json:"weight_ns"`
}

type flameResp struct {
	Program     string    `json:"program"`
	Version     string    `json:"version"`
	SampleCount int64     `json:"sample_count"`
	TotalWeight int64     `json:"total_weight_ns"`
	Root        *nodeDTO  `json:"root"`
	Functions   []funcDTO `json:"functions"`
	Gaps        []holeDTO `json:"gaps"`
	Unknowns    []holeDTO `json:"unknowns"`
}

type funcDTO struct {
	Module    string `json:"module"`
	Func      string `json:"func"`
	Self      int64  `json:"self_ns"`
	Total     int64  `json:"total_ns"`
	Count     int64  `json:"count"`
	SelfCount int64  `json:"self_count"`
}

func toNodeDTO(n *merge.Node) *nodeDTO {
	d := &nodeDTO{
		Module: n.Frame.Module, Func: n.Frame.Func,
		Self: n.Self, Total: n.Total, Count: n.Count, SelfCount: n.SelfCount,
	}
	for _, c := range n.Children {
		d.Children = append(d.Children, toNodeDTO(c))
	}
	return d
}

func toHoleDTO(hs []merge.HoleStat) []holeDTO {
	out := make([]holeDTO, 0, len(hs))
	for _, h := range hs {
		out = append(out, holeDTO{
			Module: h.Frame.Module, Func: h.Frame.Func, Kind: h.Kind,
			Samples: h.Samples, Weight: h.Weight,
		})
	}
	return out
}

func (s *Server) handleFlame(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	program, version := q.Get("program"), q.Get("version")
	if program == "" || version == "" {
		writeErr(w, http.StatusBadRequest, errMissing("program and version"))
		return
	}
	samples := s.store.Query(program, version, parseFilter(r))
	res := merge.Merge(samples, true)
	resp := flameResp{
		Program: program, Version: version,
		SampleCount: res.SampleCount, TotalWeight: res.TotalWeight,
		Root: toNodeDTO(res.Root),
		Gaps: toHoleDTO(res.Gaps), Unknowns: toHoleDTO(res.Unknowns),
	}
	resp.Functions = make([]funcDTO, 0, len(res.Functions))
	for _, f := range res.Functions {
		resp.Functions = append(resp.Functions, funcDTO{
			Module: f.Frame.Module, Func: f.Frame.Func,
			Self: f.Self, Total: f.Total, Count: f.Count, SelfCount: f.SelfCount,
		})
	}
	writeJSON(w, resp)
}

func (s *Server) handleHot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	program, version := q.Get("program"), q.Get("version")
	if program == "" || version == "" {
		writeErr(w, http.StatusBadRequest, errMissing("program and version"))
		return
	}
	samples := s.store.Query(program, version, parseFilter(r))
	res := merge.Merge(samples, true)
	type row = hotRow
	rows := make([]hotRow, 0, len(res.Functions))
	for _, f := range res.Functions {
		r := hotRow{funcDTO: funcDTO{
			Module: f.Frame.Module, Func: f.Frame.Func,
			Self: f.Self, Total: f.Total, Count: f.Count, SelfCount: f.SelfCount,
		}}
		if res.TotalWeight > 0 {
			r.SelfShare = float64(f.Self) / float64(res.TotalWeight)
			r.TotalShare = float64(f.Total) / float64(res.TotalWeight)
		}
		rows = append(rows, r)
	}
	// Hot ranking is by self time (the cost truly charged to the function),
	// then total, then name for a stable tie-break.
	sortRows(rows)
	writeJSON(w, map[string]any{
		"program": program, "version": version,
		"sample_count": res.SampleCount, "total_weight_ns": res.TotalWeight,
		"rows": rows,
	})
}

type diffRowDTO struct {
	Module     string  `json:"module"`
	Func       string  `json:"func"`
	OldCount   int64   `json:"old_count"`
	NewCount   int64   `json:"new_count"`
	OldTotal   int64   `json:"old_total_samples"`
	NewTotal   int64   `json:"new_total_samples"`
	OldShare   float64 `json:"old_share"`
	NewShare   float64 `json:"new_share"`
	DeltaShare float64 `json:"delta_share"`
	Direction  string  `json:"direction"`
}

type diffNodeDTO struct {
	Module    string         `json:"module"`
	Func      string         `json:"func"`
	OldCount  int64          `json:"old_count"`
	NewCount  int64          `json:"new_count"`
	DeltaNorm int64          `json:"delta_norm"`
	Children  []*diffNodeDTO `json:"children,omitempty"`
}

type diffResp struct {
	Program    string       `json:"program"`
	OldVersion string       `json:"old_version"`
	NewVersion string       `json:"new_version"`
	OldTotal   int64        `json:"old_total_samples"`
	NewTotal   int64        `json:"new_total_samples"`
	Rows       []diffRowDTO `json:"rows"`
	Tree       *diffNodeDTO `json:"tree"`
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	program := q.Get("program")
	oldV, newV := q.Get("old_version"), q.Get("new_version")
	if program == "" || oldV == "" || newV == "" {
		writeErr(w, http.StatusBadRequest, errMissing("program, old_version and new_version"))
		return
	}
	f := parseFilter(r)
	oldSamples := s.store.Query(program, oldV, f)
	newSamples := s.store.Query(program, newV, f)
	// Diff works on unfolded trees; Build applies its own joint folding.
	d := diff.Build(merge.Merge(oldSamples, false), merge.Merge(newSamples, false))
	resp := diffResp{
		Program: program, OldVersion: oldV, NewVersion: newV,
		OldTotal: d.OldTotal, NewTotal: d.NewTotal,
		Tree: toDiffNodeDTO(d.Tree),
	}
	resp.Rows = make([]diffRowDTO, 0, len(d.Rows))
	for _, row := range d.Rows {
		dto := diffRowDTO{
			Module: row.Frame.Module, Func: row.Frame.Func,
			OldCount: row.OldCount, NewCount: row.NewCount,
			OldTotal: row.OldTotalCount, NewTotal: row.NewTotalCount,
			OldShare:   float64(row.OldNorm) / 1e9,
			NewShare:   float64(row.NewNorm) / 1e9,
			DeltaShare: float64(row.DeltaNorm) / 1e9,
			Direction:  string(row.Direction),
		}
		resp.Rows = append(resp.Rows, dto)
	}
	writeJSON(w, resp)
}

func toDiffNodeDTO(n *diff.TreeNode) *diffNodeDTO {
	d := &diffNodeDTO{
		Module: n.Frame.Module, Func: n.Frame.Func,
		OldCount: n.OldCount, NewCount: n.NewCount, DeltaNorm: n.DeltaNorm,
	}
	for _, c := range n.Children {
		d.Children = append(d.Children, toDiffNodeDTO(c))
	}
	return d
}

type procDTO struct {
	Program  string   `json:"program"`
	Version  string   `json:"version"`
	Pid      int64    `json:"pid"`
	PeriodNs int64    `json:"period_ns"`
	Samples  int64    `json:"samples"`
	FirstNs  uint64   `json:"first_ns"`
	LastNs   uint64   `json:"last_ns"`
	Modules  int      `json:"modules"`
	Labels   []string `json:"labels"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	infos := s.store.Status()
	procs := make([]procDTO, 0, len(infos))
	var totalSamples int64
	for _, p := range infos {
		labels := make([]string, 0, len(p.Labels))
		for l := range p.Labels {
			labels = append(labels, l)
		}
		sortStrings(labels)
		procs = append(procs, procDTO{
			Program: p.Program, Version: p.Version, Pid: p.Pid,
			PeriodNs: p.PeriodNs, Samples: p.Samples,
			FirstNs: p.FirstNs, LastNs: p.LastNs,
			Modules: p.Modules, Labels: labels,
		})
		totalSamples += p.Samples
	}
	writeJSON(w, map[string]any{
		"total_samples": totalSamples,
		"processes":     procs,
		"programs":      s.store.Programs(),
	})
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

func errMissing(what string) error { return simpleError("missing required query parameters: " + what) }
