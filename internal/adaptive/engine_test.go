package adaptive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) Scenario {
	t.Helper()
	raw, err := os.ReadFile("../../examples/adaptive/replay.json")
	if err != nil {
		t.Fatal(err)
	}
	var s Scenario
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func engine(t *testing.T, s Scenario) *Engine {
	t.Helper()
	e, err := New(s.Config, s.State)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReplayAcceptedWorkAndAdverseQuality(t *testing.T) {
	s := fixture(t)
	fixed, err := Replay(s, "fixed")
	if err != nil {
		t.Fatal(err)
	}
	adaptive, err := Replay(s, "adaptive")
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Accepted != 2 || adaptive.Accepted != 4 || fixed.CostUSD != 4 || adaptive.CostUSD != 4 {
		t.Fatalf("unexpected comparison: %+v %+v", fixed, adaptive)
	}
	for _, event := range s.Events {
		o := event.Outcomes["economy"]
		o.Accepted = false
		event.Outcomes["economy"] = o
	}
	adverse, err := Replay(s, "adaptive")
	if err != nil {
		t.Fatal(err)
	}
	if adverse.Accepted != 0 || adverse.Failed != 4 {
		t.Fatalf("quality must matter: %+v", adverse)
	}
}

func TestReplayPreExecutionFailover(t *testing.T) {
	s := fixture(t)
	s.Events = s.Events[:1]
	o := s.Events[0].Outcomes["strong"]
	o.TransportFailure = true
	o.ActualCostUSD = 0
	s.Events[0].Outcomes["strong"] = o
	r, err := Replay(s, "fixed")
	if err != nil {
		t.Fatal(err)
	}
	if r.Attempts != 2 || r.Accepted != 1 || r.Decisions[1].Selected != "economy" {
		t.Fatalf("failover: %+v", r)
	}
}

func TestRejectUnsafeCandidates(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Engine, *Task, time.Time)
		reason string
	}{
		{"short_bottleneck", func(e *Engine, t *Task, n time.Time) {
			for i := range e.State.Observations {
				if e.State.Observations[i].Window == "short" {
					v := 99.0
					e.State.Observations[i].UsedPercent = &v
				}
			}
		}, "window_bottleneck"},
		{"weekly_bottleneck", func(e *Engine, t *Task, n time.Time) {
			for i := range e.State.Observations {
				if e.State.Observations[i].Window == "weekly" {
					v := 99.0
					e.State.Observations[i].UsedPercent = &v
				}
			}
		}, "window_bottleneck"},
		{"unknown_usage", func(e *Engine, t *Task, n time.Time) {
			for i := range e.State.Observations {
				e.State.Observations[i].UsedPercent = nil
			}
		}, "unknown_usage"},
		{"unknown_reset", func(e *Engine, t *Task, n time.Time) {
			for i := range e.State.Observations {
				e.State.Observations[i].ResetAt = time.Time{}
			}
		}, "unknown_or_expired_reset"},
		{"expired_reset", func(e *Engine, t *Task, n time.Time) {
			for i := range e.State.Observations {
				e.State.Observations[i].ResetAt = n
			}
		}, "unknown_or_expired_reset"},
		{"stale", func(e *Engine, t *Task, n time.Time) {
			for i := range e.State.Observations {
				e.State.Observations[i].At = e.State.Observations[i].At.Add(-time.Hour)
			}
		}, "stale_observation"},
		{"future", func(e *Engine, t *Task, n time.Time) {
			for i := range e.State.Observations {
				e.State.Observations[i].At = n.Add(time.Hour)
			}
		}, "future_observation"},
		{"capability", func(e *Engine, t *Task, n time.Time) { t.Capabilities = []string{"unverified"} }, "capability"},
		{"context", func(e *Engine, t *Task, n time.Time) { t.ContextTokens = 999999 }, "context_capacity"},
		{"deadline", func(e *Engine, t *Task, n time.Time) { t.Deadline = n.Add(time.Second) }, "deadline"},
		{"unknown_cost", func(e *Engine, t *Task, n time.Time) { t.Estimates = nil }, "missing_or_invalid_estimate"},
		{"budget", func(e *Engine, t *Task, n time.Time) { e.Config.BudgetUSD = 0.1 }, "budget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fixture(t)
			e := engine(t, s)
			task := *s.Events[0].Task
			tt.change(e, &task, s.Events[0].At)
			d := e.Decide(task, s.Events[0].At, "adaptive", nil)
			raw, _ := json.Marshal(d)
			if d.Selected != "" || !strings.Contains(string(raw), tt.reason) {
				t.Fatalf("%s", raw)
			}
		})
	}
}

func TestIsolationSharedPoolsAndAffinity(t *testing.T) {
	s := fixture(t)
	e := engine(t, s)
	task := *s.Events[0].Task
	n := s.Events[0].At
	k := s.Config.Routes[1].Windows[0]
	beforeA := e.Project(s.Config.Routes[0].Windows[0], n).RemainingPercent
	d := e.Decide(task, n, "adaptive", nil)
	if err := e.Reserve(task, d); err != nil {
		t.Fatal(err)
	}
	if e.Project(k, n).RemainingPercent != 83 || e.Project(s.Config.Routes[0].Windows[0], n).RemainingPercent != beforeA {
		t.Fatal("account isolation")
	}
	shared := e.Config.Routes[1]
	shared.ID = "other-model"
	shared.Model = "other"
	e.Config.Routes = append(e.Config.Routes, shared)
	if e.Project(shared.Windows[0], n).RemainingPercent != 83 {
		t.Fatal("model cannot refill shared pool")
	}
	task.ID = "next"
	cost := task.Estimates["economy"]
	cost.CostUSD = 3
	task.Estimates["economy"] = cost
	if d = e.Decide(task, n, "adaptive", nil); d.Selected != "economy" {
		t.Fatalf("lost session pin: %+v", d)
	}
	if d = e.Decide(task, n, "adaptive", map[string]bool{"strong": true}); d.Selected != "" {
		t.Fatal("unavailable pinned account must not switch")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	e.Config.Routes = e.Config.Routes[:2]
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(s.Config, state)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Decide(*s.Events[0].Task, n, "adaptive", nil).Reason != "duplicate_task" {
		t.Fatal("replay after restart")
	}
	changed := s.Config
	changed.BudgetUSD++
	if _, err = New(changed, state); err == nil {
		t.Fatal("policy drift must not reset ledger")
	}
}

func TestResetSegmentationAndLearnedDuration(t *testing.T) {
	s := fixture(t)
	e := engine(t, s)
	n := s.Events[0].At
	k := s.Config.Routes[0].Windows[0]
	before := e.Project(k, n)
	if before.RatePerSecond <= 0 || before.EstimatedEmptyAt == nil {
		t.Fatal("missing measured rate")
	}
	e.State.Observations = nil
	reset := n.Add(120 * time.Second)
	add := func(at, rs time.Time, used float64) {
		t.Helper()
		if err := e.Observe(Observation{Key: k, At: at, ResetAt: rs, UsedPercent: &used, Source: "fixture", IdentityEvidence: "account-bound"}); err != nil {
			t.Fatal(err)
		}
	}
	add(n, reset, 90)
	add(n.Add(time.Minute), reset, 95)
	newReset := reset.Add(5 * time.Hour)
	add(reset.Add(time.Second), newReset, 0)
	p := e.Project(k, reset.Add(time.Second))
	if p.RatePerSecond != 0 || p.DurationSeconds != 0 || p.RemainingPercent != 100 {
		t.Fatalf("new cycle polluted: %+v", p)
	}
	add(reset.Add(time.Minute), newReset, 1)
	p = e.Project(k, reset.Add(time.Minute))
	if p.DurationSeconds != 18000 || p.DurationSource != "observed_rollover" || p.RatePerSecond != 0 {
		t.Fatalf("rollover: %+v", p)
	}
	add(reset.Add(2*time.Minute), newReset, 2)
	p = e.Project(k, reset.Add(2*time.Minute))
	if p.Confidence != "observed_segment" || p.RatePerSecond > 0.02 {
		t.Fatalf("segment: %+v", p)
	}
	add(reset.Add(3*time.Minute), newReset, 0)
	if e.Project(k, reset.Add(3*time.Minute)).RatePerSecond != 0 {
		t.Fatal("counter regression must segment history")
	}
}

func TestMissingIdentityAndAllUnavailable(t *testing.T) {
	s := fixture(t)
	e := engine(t, s)
	o := s.State.Observations[0]
	o.At = o.At.Add(time.Hour)
	o.IdentityEvidence = "provider-only"
	if e.Observe(o) == nil {
		t.Fatal("provider-only identity accepted")
	}
	o.IdentityEvidence = "account-bound"
	o.Account = "another-account"
	if e.Observe(o) == nil {
		t.Fatal("unconfigured account accepted")
	}
	d := e.Decide(*s.Events[0].Task, s.Events[0].At, "adaptive", map[string]bool{})
	if d.Selected != "" {
		t.Fatal("all unavailable")
	}
}

func TestTokenBarDoesNotBecomeAccountQuota(t *testing.T) {
	raw := []byte(`{"agents":[{"clientId":"codex","source":"fixture","updatedAt":"2026-09-10T04:00:00Z","windows":[{"resetsAt":"2026-09-10T09:00:00Z","paceStatus":{"state":"available","windowKey":"short","durationSeconds":18000,"durationSource":"observed"},"historicalPace":{"etaSeconds":600}}]}]}`)
	rows, err := InspectTokenBar(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Routable || *rows[0].EstimatedETASeconds != 600 || *rows[0].DurationSeconds != 18000 || rows[0].ResetAt == "" {
		t.Fatalf("%+v", rows)
	}
}
