package adaptive

import (
	"encoding/json"
	"fmt"
)

type TokenBarInspection struct {
	ClientID            string   `json:"client_id"`
	Source              string   `json:"source"`
	UpdatedAt           string   `json:"updated_at"`
	WindowKey           string   `json:"window_key"`
	ResetAt             string   `json:"measured_reset_at"`
	DurationSeconds     *float64 `json:"duration_seconds"`
	DurationSource      string   `json:"duration_source"`
	EstimatedETASeconds *float64 `json:"estimated_eta_seconds"`
	PaceState           string   `json:"pace_state"`
	Routable            bool     `json:"routable"`
	Reason              string   `json:"reason"`
}

func InspectTokenBar(raw []byte) ([]TokenBarInspection, error) {
	var wrapper struct {
		Payload json.RawMessage `json:"payload"`
		Data    json.RawMessage `json:"data"`
		OK      *bool           `json:"ok"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	if wrapper.OK != nil && !*wrapper.OK {
		return nil, fmt.Errorf("TokenBar returned an error envelope")
	}
	if len(wrapper.Payload) > 0 {
		raw = wrapper.Payload
	} else if len(wrapper.Data) > 0 {
		raw = wrapper.Data
	}
	var payload struct {
		Agents []struct {
			ClientID  string `json:"clientId"`
			Source    string `json:"source"`
			UpdatedAt string `json:"updatedAt"`
			Windows   []struct {
				ResetAt string `json:"resetsAt"`
				Pace    struct {
					State           string   `json:"state"`
					WindowKey       string   `json:"windowKey"`
					DurationSeconds *float64 `json:"durationSeconds"`
					DurationSource  string   `json:"durationSource"`
				} `json:"paceStatus"`
				Historical struct {
					ETA *float64 `json:"etaSeconds"`
				} `json:"historicalPace"`
			} `json:"windows"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Agents == nil {
		return nil, fmt.Errorf("missing TokenBar agents payload")
	}
	out := []TokenBarInspection{}
	for _, a := range payload.Agents {
		for _, w := range a.Windows {
			out = append(out, TokenBarInspection{ClientID: a.ClientID, Source: a.Source, UpdatedAt: a.UpdatedAt, WindowKey: w.Pace.WindowKey, ResetAt: w.ResetAt, DurationSeconds: w.Pace.DurationSeconds, DurationSource: w.Pace.DurationSource, EstimatedETASeconds: w.Historical.ETA, PaceState: w.Pace.State, Reason: "account_scope_not_exported; provider_history_is_not_credential_evidence"})
		}
	}
	return out, nil
}
