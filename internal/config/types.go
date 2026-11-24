package config

import "time"

const AutoNotificationHelper = "auto"

const (
	GroupModeChain    = "chain"
	GroupModeSelector = "selector"
)

type Config struct {
	Mihomo        Core          `yaml:"mihomo"`
	Schedule      Schedule      `yaml:"schedule"`
	Speed         Speed         `yaml:"speedtest"`
	Selection     Selection     `yaml:"selection"`
	History       History       `yaml:"history"`
	Notifications Notifications `yaml:"notifications"`
	Groups        []Group       `yaml:"groups"`
}
type Core struct {
	Controller      string `yaml:"controller"`
	Secret          string `yaml:"secret"`
	ConfigPath      string `yaml:"config_path"`
	StableBinary    string `yaml:"stable_binary"`
	AllowPrerelease bool   `yaml:"allow_prerelease"`
	TestGroup       string `yaml:"test_group"`
	TestProxy       string `yaml:"test_proxy"`
}
type Schedule struct {
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	HeartbeatTimeout  time.Duration `yaml:"heartbeat_timeout"`
	HeartbeatFailures int           `yaml:"heartbeat_failures"`
	FullInterval      time.Duration `yaml:"full_interval"`
	ActiveInterval    time.Duration `yaml:"active_interval"`
	RecheckDelay      time.Duration `yaml:"recheck_delay"`
	Cooldown          time.Duration `yaml:"cooldown"`
	GeoInterval       time.Duration `yaml:"geo_interval"`
}
type Speed struct {
	LegacyStreams       *int          `yaml:"streams_per_node,omitempty"`
	BenchStreams        int           `yaml:"bench_streams"`
	BenchBytesPerStream int64         `yaml:"bench_bytes_per_stream"`
	URL                 string        `yaml:"url"`
	LivenessURL         string        `yaml:"liveness_url"`
	GeoURL              string        `yaml:"geo_url"`
	TraceURL            string        `yaml:"trace_url"`
	Timeout             time.Duration `yaml:"timeout"`
	SweepBytes          int64         `yaml:"sweep_bytes"`
	QuickBytes          int64         `yaml:"quick_bytes"`
	ConfirmBytes        int64         `yaml:"confirm_bytes"`
	MonthlyBytes        int64         `yaml:"monthly_bytes"`
	BillingDay          int           `yaml:"billing_day"`
}
type Selection struct {
	SevereDropPct     float64           `yaml:"severe_drop_pct"`
	NearBestPct       float64           `yaml:"near_best_pct"`
	SlowConfirmations int               `yaml:"slow_confirmations"`
	SyncDirect        bool              `yaml:"sync_direct_on_warp_switch"`
	FollowGroups      map[string]string `yaml:"follow_groups"`
	MaxAge            time.Duration     `yaml:"max_sample_age"`
}

// Follower returns the selector to mirror after a verified primary change.
func (s Selection) Follower(g Group) string {
	if !s.SyncDirect {
		return ""
	}
	if name := s.FollowGroups[g.Name]; name != "" {
		return name
	}
	if g.Mode == GroupModeSelector {
		return ""
	}
	return g.Direct
}

type History struct {
	File           string `yaml:"file"`
	SamplesPerNode int    `yaml:"samples_per_node"`
}
type Notifications struct {
	Enabled bool   `yaml:"enabled"`
	Summary bool   `yaml:"summary"`
	Switch  bool   `yaml:"switch"`
	Helper  string `yaml:"helper"`
}
type Group struct {
	Name        string  `yaml:"name"`
	Mode        string  `yaml:"mode"`
	Chain       string  `yaml:"chain"`
	Direct      string  `yaml:"direct"`
	GeoProxy    string  `yaml:"geo_proxy"`
	ExpectedGeo string  `yaml:"expected_geo"`
	StrictGeo   bool    `yaml:"strict_geo"`
	Exclude     string  `yaml:"exclude_regex"`
	MinMbps     float64 `yaml:"min_mbps"`
}

// ProbeTarget is the live Mihomo route to verify after a selection.
func (g Group) ProbeTarget() string {
	if g.Mode == GroupModeSelector {
		return g.Name
	}
	return g.Chain
}
