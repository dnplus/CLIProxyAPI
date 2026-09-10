package adaptive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

type Engine struct {
	Config Config
	State  State
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func (k Key) Valid() bool {
	return k.Provider != "" && k.Account != "" && k.Pool != "" && k.Window != ""
}

func New(cfg Config, state State) (*Engine, error) {
	if !finite(cfg.BudgetUSD) || cfg.BudgetUSD <= 0 || !finite(cfg.MaxAgeSeconds) || cfg.MaxAgeSeconds <= 0 || len(cfg.Routes) == 0 {
		return nil, fmt.Errorf("positive budget, freshness limit and routes required")
	}
	ids := map[string]bool{}
	bindings := map[string]string{}
	for _, r := range cfg.Routes {
		if r.ID == "" || ids[r.ID] || r.AuthID == "" || r.Model == "" || r.ContextTokens <= 0 || len(r.Windows) == 0 || (r.SourceKind != "official-api" && r.SourceKind != "permitted-api") {
			return nil, fmt.Errorf("invalid or duplicate route %q", r.ID)
		}
		ids[r.ID] = true
		binding := r.Provider + "/" + r.Account
		if previous, ok := bindings[r.AuthID]; ok && previous != binding {
			return nil, fmt.Errorf("auth ID has conflicting account binding")
		}
		bindings[r.AuthID] = binding
		seen := map[string]bool{}
		for _, k := range r.Windows {
			if !k.Valid() || k.Provider != r.Provider || k.Account != r.Account || seen[k.Window] {
				return nil, fmt.Errorf("invalid window binding in %q", r.ID)
			}
			seen[k.Window] = true
		}
	}
	if len(state.Reservations) > 10000 || len(state.Sessions) > 10000 {
		return nil, fmt.Errorf("state capacity exceeded")
	}
	rawConfig, _ := json.Marshal(cfg)
	digest := sha256.Sum256(rawConfig)
	configDigest := hex.EncodeToString(digest[:])
	if state.ConfigDigest != "" && state.ConfigDigest != configDigest {
		return nil, fmt.Errorf("policy changed; explicit ledger reconciliation required")
	}
	if state.ConfigDigest == "" && (len(state.Reservations) > 0 || len(state.Sessions) > 0) {
		return nil, fmt.Errorf("ledger missing policy binding")
	}
	state.ConfigDigest = configDigest
	tasks := map[string]bool{}
	for session, route := range state.Sessions {
		if session == "" || !ids[route] {
			return nil, fmt.Errorf("invalid session ledger")
		}
	}
	for _, r := range state.Reservations {
		if r.TaskID == "" || tasks[r.TaskID] || !ids[r.RouteID] || !finite(r.CostUSD) || r.CostUSD <= 0 || len(r.Windows) == 0 {
			return nil, fmt.Errorf("invalid reservation ledger")
		}
		tasks[r.TaskID] = true
		for _, w := range r.Windows {
			if !w.Key.Valid() || w.ResetAt.IsZero() || w.UsedPercent == nil || !finite(*w.UsedPercent) || *w.UsedPercent <= 0 || *w.UsedPercent > 100 {
				return nil, fmt.Errorf("invalid reserved window")
			}
		}
	}
	if state.Sessions == nil {
		state.Sessions = map[string]string{}
	}
	e := &Engine{Config: cfg, State: state}
	e.State.Observations = nil
	for _, o := range state.Observations {
		if err := e.Observe(o); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func (e *Engine) Observe(o Observation) error {
	if !o.Key.Valid() || o.At.IsZero() || o.Source == "" || o.IdentityEvidence != "account-bound" || (o.UsedPercent != nil && (!finite(*o.UsedPercent) || *o.UsedPercent < 0 || *o.UsedPercent > 100)) {
		return fmt.Errorf("observation requires account-bound identity, source, timestamp and valid usage")
	}
	known := false
	for _, r := range e.Config.Routes {
		if slices.Contains(r.Windows, o.Key) {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("unconfigured account/window")
	}
	for _, prev := range e.State.Observations {
		if prev.Key == o.Key && !prev.At.Before(o.At) {
			a, _ := json.Marshal(prev)
			b, _ := json.Marshal(o)
			if string(a) == string(b) {
				return nil
			}
			return fmt.Errorf("out-of-order or conflicting observation")
		}
	}
	e.State.Observations = append(e.State.Observations, o)
	count := 0
	for i := len(e.State.Observations) - 1; i >= 0; i-- {
		if e.State.Observations[i].Key == o.Key {
			count++
			if count > 64 {
				e.State.Observations = append(e.State.Observations[:i], e.State.Observations[i+1:]...)
			}
		}
	}
	return nil
}

func (e *Engine) Project(k Key, now time.Time) Projection {
	p := Projection{Key: k, Confidence: "unknown", Reason: "missing_observation"}
	var history []Observation
	for _, o := range e.State.Observations {
		if o.Key == k {
			history = append(history, o)
		}
	}
	if len(history) == 0 {
		return p
	}
	o := history[len(history)-1]
	p.Source, p.ObservedAt, p.ResetAt = o.Source, o.At, o.ResetAt
	if o.At.After(now) {
		p.Reason = "future_observation"
		return p
	}
	if now.Sub(o.At).Seconds() > e.Config.MaxAgeSeconds {
		p.Reason = "stale_observation"
		return p
	}
	if o.UsedPercent == nil {
		p.Reason = "unknown_usage"
		return p
	}
	if !o.ResetAt.After(now) {
		p.Reason = "unknown_or_expired_reset"
		return p
	}
	p.RemainingPercent = 100 - *o.UsedPercent
	p.Confidence, p.Reason = "snapshot_only", ""
	if o.DurationSeconds > 0 && finite(o.DurationSeconds) && o.DurationSeconds <= 400*86400 && (o.DurationSource == "provider" || o.DurationSource == "contract") && o.ResetAt.Sub(o.At).Seconds() <= o.DurationSeconds {
		p.DurationSeconds, p.DurationSource = o.DurationSeconds, o.DurationSource
	}
	start := len(history) - 1
	for start > 0 {
		prev, next := history[start-1], history[start]
		if !prev.ResetAt.Equal(o.ResetAt) || prev.UsedPercent == nil || next.UsedPercent == nil || *next.UsedPercent < *prev.UsedPercent || next.At.Sub(prev.At).Seconds() > e.Config.MaxAgeSeconds {
			break
		}
		start--
	}
	if p.DurationSeconds == 0 && start >= 2 && len(history)-start >= 2 {
		old, stable := history[start-1], history[start-2]
		first := history[start]
		delta := o.ResetAt.Sub(old.ResetAt).Seconds()
		if old.ResetAt.Equal(stable.ResetAt) && old.At.Before(old.ResetAt) && old.ResetAt.Sub(old.At) <= 15*time.Minute && !first.At.Before(old.ResetAt) && first.At.Sub(old.ResetAt) <= 15*time.Minute && history[start+1].At.Sub(first.At) <= 15*time.Minute && delta > 0 && delta <= 400*86400 {
			p.DurationSeconds, p.DurationSource = delta, "observed_rollover"
		}
	}
	if len(history)-start >= 3 && o.At.Sub(history[start].At) >= time.Minute {
		for i := start + 1; i < len(history); i++ {
			a, b := history[i-1], history[i]
			rate := (*b.UsedPercent - *a.UsedPercent) / b.At.Sub(a.At).Seconds()
			p.RatePerSecond = math.Max(p.RatePerSecond, rate)
		}
		p.Confidence = "observed_segment"
	}
	for _, r := range e.State.Reservations {
		for _, w := range r.Windows {
			cycleStart := o.ResetAt.Add(-time.Duration(p.DurationSeconds * float64(time.Second)))
			renewed := p.DurationSeconds > 0 && !cycleStart.Before(w.At) && o.At.After(w.At)
			if w.Key == k && !renewed {
				p.RemainingPercent -= *w.UsedPercent
			}
		}
	}
	p.RemainingPercent = math.Max(0, p.RemainingPercent-p.RatePerSecond*now.Sub(o.At).Seconds())
	if p.RatePerSecond > 0 {
		seconds := p.RemainingPercent / p.RatePerSecond
		if seconds <= 400*86400 {
			t := now.Add(time.Duration(seconds * float64(time.Second)))
			p.EstimatedEmptyAt = &t
		}
	}
	return p
}

func (e *Engine) Decide(t Task, now time.Time, policy string, available map[string]bool) Decision {
	d := Decision{TaskID: t.ID, At: now, Policy: policy, Reason: "no_eligible_candidate", Candidates: []Candidate{}}
	if policy != "adaptive" && policy != "fixed" {
		d.Reason = "invalid_policy"
		return d
	}
	if t.ID == "" || t.Session == "" || t.ContextTokens <= 0 || !t.Deadline.After(now) {
		d.Reason = "invalid_task_or_deadline"
		return d
	}
	for _, r := range e.State.Reservations {
		if r.TaskID == t.ID {
			d.Reason = "duplicate_task"
			return d
		}
	}
	if len(e.State.Reservations) >= 10000 || len(e.State.Sessions) >= 10000 {
		d.Reason = "state_capacity"
		return d
	}
	spent := 0.0
	for _, r := range e.State.Reservations {
		spent += r.CostUSD
	}
	for _, r := range e.Config.Routes {
		c := Candidate{RouteID: r.ID, Eligible: true, Headroom: math.MaxFloat64, ExpiringSurplus: math.MaxFloat64, Reasons: []string{}, Windows: []Projection{}}
		reject := func(reason string) { c.Eligible = false; c.Reasons = append(c.Reasons, reason) }
		if available != nil && !available[r.ID] {
			reject("host_candidate_unavailable")
		}
		if pinned := e.State.Sessions[t.Session]; pinned != "" && pinned != r.ID {
			reject("session_pinned_elsewhere")
		}
		if r.ContextTokens < t.ContextTokens {
			reject("context_capacity")
		}
		for _, cap := range t.Capabilities {
			if !slices.Contains(r.Capabilities, cap) {
				reject("capability:" + cap)
			}
		}
		est, ok := t.Estimates[r.ID]
		if !ok || !finite(est.CostUSD) || est.CostUSD <= 0 || !finite(est.Seconds) || est.Seconds <= 0 || est.Seconds > 86400 {
			reject("missing_or_invalid_estimate")
			est = Estimate{}
		}
		c.CostUSD = est.CostUSD
		if spent+est.CostUSD > e.Config.BudgetUSD+1e-9 {
			reject("budget")
		}
		if est.Seconds > t.Deadline.Sub(now).Seconds() {
			reject("deadline")
		}
		for _, k := range r.Windows {
			p := e.Project(k, now)
			c.Windows = append(c.Windows, p)
			if p.Reason != "" {
				reject(p.Reason + ":" + k.Window)
				continue
			}
			need, known := est.WindowPercent[k.Window]
			if !known || !finite(need) || need <= 0 || need > 100 {
				reject("unknown_window_cost:" + k.Window)
				continue
			}
			if p.ResetAt.Sub(now).Seconds() < est.Seconds {
				reject("reset_during_task:" + k.Window)
			}
			headroom := (p.RemainingPercent - p.RatePerSecond*est.Seconds) / need
			c.Headroom = math.Min(c.Headroom, headroom)
			if headroom < 1 {
				reject("window_bottleneck:" + k.Window)
			}
			surplus := math.Max(0, p.RemainingPercent-p.RatePerSecond*p.ResetAt.Sub(now).Seconds()) / need
			c.ExpiringSurplus = math.Min(c.ExpiringSurplus, surplus/math.Max(1, p.ResetAt.Sub(now).Seconds()))
		}
		if c.Headroom == math.MaxFloat64 {
			c.Headroom = 0
		}
		if c.ExpiringSurplus == math.MaxFloat64 {
			c.ExpiringSurplus = 0
		}
		d.Candidates = append(d.Candidates, c)
	}
	var ranked []Candidate
	for _, c := range d.Candidates {
		if c.Eligible {
			ranked = append(ranked, c)
		}
	}
	if policy == "adaptive" {
		sort.SliceStable(ranked, func(i, j int) bool {
			a, b := ranked[i], ranked[j]
			if a.CostUSD != b.CostUSD {
				return a.CostUSD < b.CostUSD
			}
			if a.ExpiringSurplus != b.ExpiringSurplus {
				return a.ExpiringSurplus > b.ExpiringSurplus
			}
			return a.Headroom > b.Headroom
		})
	}
	if len(ranked) > 0 {
		d.Selected = ranked[0].RouteID
		d.Reason = "eligible_then_cost_expiring_surplus_headroom"
		if policy == "fixed" {
			d.Reason = "first_eligible_fixed_order"
		}
		if e.State.Sessions[t.Session] != "" {
			d.Reason = "session_affinity"
		}
	}
	return d
}

func (e *Engine) Reserve(t Task, d Decision) error {
	check := e.Decide(t, d.At, d.Policy, nil)
	if d.Selected == "" {
		return fmt.Errorf("no selection")
	}
	eligible := false
	for _, c := range check.Candidates {
		if c.RouteID == d.Selected && c.Eligible {
			eligible = true
		}
	}
	if !eligible {
		return fmt.Errorf("decision is no longer eligible")
	}
	r := e.Route(d.Selected)
	est := t.Estimates[r.ID]
	reservation := Reservation{TaskID: t.ID, RouteID: r.ID, CostUSD: est.CostUSD}
	for _, k := range r.Windows {
		p := e.Project(k, d.At)
		need := est.WindowPercent[k.Window]
		reservation.Windows = append(reservation.Windows, Observation{Key: k, At: d.At, ResetAt: p.ResetAt, UsedPercent: &need})
	}
	e.State.Reservations = append(e.State.Reservations, reservation)
	e.State.Sessions[t.Session] = r.ID
	return nil
}

func (e *Engine) Route(id string) Route {
	for _, r := range e.Config.Routes {
		if r.ID == id {
			return r
		}
	}
	return Route{}
}

func (e *Engine) Save(path string) error {
	b, err := json.MarshalIndent(e.State, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
