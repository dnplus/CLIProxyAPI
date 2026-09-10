package adaptive

import "time"

type Key struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Pool     string `json:"pool"`
	Window   string `json:"window"`
}

type Observation struct {
	Key
	At               time.Time `json:"at"`
	UsedPercent      *float64  `json:"used_percent"`
	ResetAt          time.Time `json:"reset_at"`
	Source           string    `json:"source"`
	IdentityEvidence string    `json:"identity_evidence"`
	DurationSeconds  float64   `json:"duration_seconds,omitempty"`
	DurationSource   string    `json:"duration_source,omitempty"`
}

type Route struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	Account       string   `json:"account"`
	AuthID        string   `json:"auth_id"`
	Model         string   `json:"model"`
	SourceKind    string   `json:"source_kind"`
	Capabilities  []string `json:"capabilities"`
	ContextTokens int      `json:"context_tokens"`
	Windows       []Key    `json:"windows"`
}

type Estimate struct {
	CostUSD       float64            `json:"cost_usd"`
	Seconds       float64            `json:"seconds"`
	WindowPercent map[string]float64 `json:"window_percent"`
}

type Task struct {
	ID            string              `json:"id"`
	Session       string              `json:"session"`
	Capabilities  []string            `json:"capabilities"`
	ContextTokens int                 `json:"context_tokens"`
	Deadline      time.Time           `json:"deadline"`
	Estimates     map[string]Estimate `json:"estimates"`
}

type Config struct {
	BudgetUSD     float64 `json:"budget_usd"`
	MaxAgeSeconds float64 `json:"max_age_seconds"`
	Routes        []Route `json:"routes"`
}

type Reservation struct {
	TaskID  string        `json:"task_id"`
	RouteID string        `json:"route_id"`
	CostUSD float64       `json:"cost_usd"`
	Windows []Observation `json:"windows"`
}

type State struct {
	ConfigDigest string            `json:"config_digest,omitempty"`
	Observations []Observation     `json:"observations"`
	Reservations []Reservation     `json:"reservations"`
	Sessions     map[string]string `json:"sessions"`
}

type Projection struct {
	Key
	Source           string     `json:"source"`
	ObservedAt       time.Time  `json:"observed_at"`
	ResetAt          time.Time  `json:"reset_at"`
	DurationSeconds  float64    `json:"duration_seconds,omitempty"`
	DurationSource   string     `json:"duration_source,omitempty"`
	RemainingPercent float64    `json:"remaining_percent"`
	RatePerSecond    float64    `json:"rate_per_second"`
	EstimatedEmptyAt *time.Time `json:"estimated_empty_at,omitempty"`
	Confidence       string     `json:"confidence"`
	Reason           string     `json:"reason,omitempty"`
}

type Candidate struct {
	RouteID         string       `json:"route_id"`
	Eligible        bool         `json:"eligible"`
	Reasons         []string     `json:"reasons"`
	Windows         []Projection `json:"windows"`
	CostUSD         float64      `json:"cost_usd"`
	Headroom        float64      `json:"headroom"`
	ExpiringSurplus float64      `json:"expiring_surplus"`
}

type Decision struct {
	TaskID     string      `json:"task_id"`
	At         time.Time   `json:"at"`
	Policy     string      `json:"policy"`
	Selected   string      `json:"selected,omitempty"`
	Reason     string      `json:"reason"`
	Candidates []Candidate `json:"candidates"`
}
