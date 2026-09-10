package adaptive

import (
	"encoding/json"
	"fmt"
	"time"
)

type Outcome struct {
	Accepted         bool    `json:"accepted"`
	TransportFailure bool    `json:"transport_failure,omitempty"`
	ActualCostUSD    float64 `json:"actual_cost_usd"`
	Seconds          float64 `json:"seconds"`
}

type Event struct {
	At          time.Time          `json:"at"`
	Observation *Observation       `json:"observation,omitempty"`
	Task        *Task              `json:"task,omitempty"`
	Outcomes    map[string]Outcome `json:"outcomes,omitempty"`
}

type Scenario struct {
	Evidence string  `json:"evidence"`
	Config   Config  `json:"config"`
	State    State   `json:"state"`
	Events   []Event `json:"events"`
}

type ReplayResult struct {
	Policy      string     `json:"policy"`
	Evidence    string     `json:"evidence"`
	Accepted    int        `json:"accepted"`
	Rejected    int        `json:"rejected"`
	Failed      int        `json:"failed"`
	Attempts    int        `json:"attempts"`
	CostUSD     float64    `json:"actual_cost_usd"`
	ReservedUSD float64    `json:"reserved_usd"`
	Decisions   []Decision `json:"decisions"`
}

func Replay(s Scenario, policy string) (ReplayResult, error) {
	result := ReplayResult{Policy: policy, Evidence: s.Evidence, Decisions: []Decision{}}
	if s.Evidence != "synthetic" && s.Evidence != "replay" {
		return result, fmt.Errorf("scenario must identify synthetic or replay evidence")
	}
	raw, err := json.Marshal(s.State)
	if err != nil {
		return result, err
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		return result, err
	}
	e, err := New(s.Config, state)
	if err != nil {
		return result, err
	}
	var last time.Time
	for _, event := range s.Events {
		if event.At.Before(last) {
			return result, fmt.Errorf("events out of order")
		}
		last = event.At
		if event.Observation != nil {
			if event.Observation.At.After(event.At) {
				return result, fmt.Errorf("future observation")
			}
			if err = e.Observe(*event.Observation); err != nil {
				return result, err
			}
		}
		if event.Task == nil {
			continue
		}
		available := map[string]bool{}
		for _, r := range s.Config.Routes {
			available[r.ID] = true
		}
		for {
			d := e.Decide(*event.Task, event.At, policy, available)
			result.Decisions = append(result.Decisions, d)
			if d.Selected == "" {
				result.Rejected++
				break
			}
			outcome, ok := event.Outcomes[d.Selected]
			if !ok || outcome.ActualCostUSD < 0 || !finite(outcome.ActualCostUSD) || outcome.Seconds <= 0 || !finite(outcome.Seconds) {
				return result, fmt.Errorf("missing or invalid independent outcome for %q", d.Selected)
			}
			result.Attempts++
			if outcome.TransportFailure {
				if outcome.ActualCostUSD != 0 {
					return result, fmt.Errorf("failover fixture must be pre-execution with zero cost")
				}
				available[d.Selected] = false
				if e.State.Sessions[event.Task.Session] != "" {
					result.Failed++
					break
				}
				continue
			}
			if err = e.Reserve(*event.Task, d); err != nil {
				return result, err
			}
			result.CostUSD += outcome.ActualCostUSD
			if outcome.Accepted && outcome.Seconds <= event.Task.Deadline.Sub(event.At).Seconds() && result.CostUSD <= s.Config.BudgetUSD+1e-9 {
				result.Accepted++
			} else {
				result.Failed++
			}
			break
		}
	}
	for _, r := range e.State.Reservations {
		result.ReservedUSD += r.CostUSD
	}
	return result, nil
}
