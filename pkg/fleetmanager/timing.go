package fleetmanager

import (
	"math"

	"github.com/xuhuanhello/nakama-agones/internal/state"
)

func validTimingMetrics(m state.Metrics) bool {
	for _, n := range []*int{m.SimulationWorkers, m.AuditWorkers} {
		if n != nil && (*n < 1 || *n > 8) {
			return false
		}
	}
	for _, w := range []*state.LatencyWindow{m.ClientPresentationToReadyMS, m.ClientPresentationToSettlementMS, m.ServerFirstACKToReadyMS, m.ServerFirstACKToSettlementMS, m.SimulationQueueMS, m.SimulationWorkMS, m.ServerLastACKToReadyMS, m.ServerLastACKToSettlementMS} {
		if w == nil {
			continue
		}
		if w.WindowSeconds != 60 || w.Count < 0 || w.Count > 100000 {
			return false
		}
		values := []*float64{w.P50, w.P95, w.P99, w.Max, w.LastSampleAgeSeconds}
		for _, v := range values {
			if w.Count == 0 {
				if v != nil {
					return false
				}
				continue
			}
			if v == nil || *v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0) {
				return false
			}
		}
		if w.Count > 0 && (*w.P50 > *w.P95 || *w.P95 > *w.P99 || *w.P99 > *w.Max || *w.LastSampleAgeSeconds > 60) {
			return false
		}
	}
	return true
}
