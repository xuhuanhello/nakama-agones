package state

// CapacityPolicy is a desired template for newly requested game processes.
// Each worker pins its own copy; changing the template never mutates a live pod.
type CapacityPolicy struct {
	Revision             int64  `json:"revision"`
	RoomsPerInstance     int    `json:"rooms_per_instance"`
	CPURequestMillicores int    `json:"cpu_request_millicores"`
	CPULimitMillicores   int    `json:"cpu_limit_millicores"`
	UpdatedAt            int64  `json:"updated_at"`
	UpdatedBy            string `json:"updated_by"`
}
type PolicyAudit struct {
	At     int64          `json:"at"`
	Actor  string         `json:"actor"`
	Before CapacityPolicy `json:"before"`
	After  CapacityPolicy `json:"after"`
}

// LatencyWindow represents an explicitly named 60-second timing stage. Nil
// quantiles are unavailable, never a zero millisecond success measurement.
type LatencyWindow struct {
	WindowSeconds        int      `json:"window_seconds"`
	Count                int      `json:"count"`
	P50                  *float64 `json:"p50"`
	P95                  *float64 `json:"p95"`
	P99                  *float64 `json:"p99"`
	Max                  *float64 `json:"max"`
	LastSampleAgeSeconds *float64 `json:"last_sample_age_seconds"`
}
