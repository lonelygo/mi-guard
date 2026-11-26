package history

import "time"

// Decision is the durable explanation of one guard selection outcome. It is
// intentionally self-contained so a notification can reopen the exact event
// after the live controller state has changed.
type Decision struct {
	ID            string              `json:"id"`
	At            time.Time           `json:"at"`
	Source        string              `json:"source"`
	Group         string              `json:"group"`
	Chain         string              `json:"chain,omitempty"`
	Before        string              `json:"before,omitempty"`
	After         string              `json:"after,omitempty"`
	Action        string              `json:"action"`
	Reason        string              `json:"reason,omitempty"`
	Trigger       string              `json:"trigger,omitempty"`
	NearBestPct   float64             `json:"nearBestPct,omitempty"`
	ThresholdMbps *float64            `json:"thresholdMbps,omitempty"`
	MinMbps       float64             `json:"minMbps,omitempty"`
	Candidates    []DecisionCandidate `json:"candidates,omitempty"`
	Followers     []DecisionFollower  `json:"followers,omitempty"`
}

type DecisionCandidate struct {
	Node             string     `json:"node"`
	Provider         string     `json:"provider,omitempty"`
	Kind             string     `json:"kind,omitempty"`
	ObservedAt       *time.Time `json:"observedAt,omitempty"`
	Target           string     `json:"target,omitempty"`
	Mbps             float64    `json:"mbps,omitempty"`
	LatencyMS        int        `json:"latencyMS,omitempty"`
	Attempts         int        `json:"attempts,omitempty"`
	Failures         int        `json:"failures,omitempty"`
	Streams          int        `json:"streams,omitempty"`
	RequestedBytes   int64      `json:"requestedBytes,omitempty"`
	Bytes            int64      `json:"bytes,omitempty"`
	Stage            string     `json:"stage,omitempty"`
	Status           string     `json:"status,omitempty"`
	Rank             int        `json:"rank,omitempty"`
	PreferredBand    *bool      `json:"preferredBand,omitempty"`
	StrategyAttempts int        `json:"strategyAttempts,omitempty"`
	StrategyFailures int        `json:"strategyFailures,omitempty"`
	HistoryAttempts  int        `json:"historyAttempts,omitempty"`
	HistoryFailures  int        `json:"historyFailures,omitempty"`
	HistorySamples   int        `json:"historySamples,omitempty"`
	HistoryP25Mbps   *float64   `json:"historyP25Mbps,omitempty"`
	ValidationOrder  int        `json:"validationOrder,omitempty"`
	Validation       string     `json:"validation,omitempty"`
	Selected         bool       `json:"selected,omitempty"`
}

type DecisionFollower struct {
	Group  string `json:"group"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}
