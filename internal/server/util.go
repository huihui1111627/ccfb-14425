package server

import "sort"

// hotRow is the local anonymous shape used by handleHot; kept as a named
// type so its ordering rules live in one place.
type hotRow struct {
	funcDTO
	SelfShare  float64 `json:"self_share"`
	TotalShare float64 `json:"total_share"`
}

// sortRows ranks hot functions by self time, then cumulative time, then
// name — all deterministic tie-breaks.
func sortRows(rows []hotRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Self != rows[j].Self {
			return rows[i].Self > rows[j].Self
		}
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		if rows[i].Func != rows[j].Func {
			return rows[i].Func < rows[j].Func
		}
		return rows[i].Module < rows[j].Module
	})
}

func sortStrings(xs []string) { sort.Strings(xs) }
