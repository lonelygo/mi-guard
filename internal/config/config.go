package config

import (
	_ "embed"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v2"
)

//go:embed defaults.yaml
var example []byte

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mihomo-guard", "mihomo-guard.yaml")
}

func WriteExample(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(example)
	return errors.Join(err, f.Close())
}

func Load(path string) (Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(example, &c); err != nil {
		return c, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("%w (create with mihomo-guard init -c %q)", err, path)
	}
	if err != nil {
		return c, err
	}
	if err = yaml.UnmarshalStrict(b, &c); err != nil {
		return c, fmt.Errorf("guard YAML: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return c, err
	}
	helperPath := &c.Notifications.Helper
	for _, p := range []*string{&c.Mihomo.ConfigPath, &c.Mihomo.StableBinary, &c.History.File, helperPath} {
		if p == helperPath && *p == AutoNotificationHelper {
			continue
		}
		if *p == "" {
			return c, errors.New("empty file path in guard configuration")
		}
		if strings.HasPrefix(*p, "~/") {
			*p = filepath.Join(home, (*p)[2:])
		}
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(filepath.Dir(path), *p)
		}
		*p, err = filepath.Abs(*p)
		if err != nil {
			return c, err
		}
	}
	if err = c.validate(); err != nil {
		return c, err
	}
	if c.Mihomo.Secret == "" {
		if live, readErr := ReadCoreFile(c.Mihomo.ConfigPath); readErr == nil {
			c.Mihomo.Secret = live.Secret
		}
	}
	return c, nil
}

func (c Config) validate() error {
	if err := localURL(c.Mihomo.Controller, false); err != nil {
		return fmt.Errorf("mihomo.controller: %w", err)
	}
	if err := localURL(c.Mihomo.TestProxy, true); err != nil {
		return fmt.Errorf("mihomo.test_proxy: %w", err)
	}
	s := c.Schedule
	if s.HeartbeatInterval <= 0 || s.HeartbeatTimeout <= 0 || s.HeartbeatFailures < 1 || s.FullInterval <= 0 || s.ActiveInterval < 0 || s.RecheckDelay <= 0 || s.Cooldown < 0 || s.GeoInterval <= 0 {
		return errors.New("invalid schedule interval or heartbeat_failures")
	}
	p := c.Selection
	if !pct(p.SevereDropPct) || p.SevereDropPct == 0 || p.SevereDropPct == 100 || !pct(p.NearBestPct) || p.SlowConfirmations < 2 || p.MaxAge <= 0 {
		return errors.New("invalid selection percentage, confirmations (minimum 2) or max_sample_age")
	}
	t := c.Speed
	if t.Streams < 1 || t.Streams > 8 {
		return errors.New("streams_per_node must be from 1 to 8")
	}
	if t.Timeout <= 0 || t.SweepBytes <= 0 || t.QuickBytes <= 0 || t.ConfirmBytes <= 0 || t.MonthlyBytes <= 0 || t.BillingDay < 1 || t.BillingDay > 28 || c.History.SamplesPerNode < 1 {
		return errors.New("invalid speedtest limits, billing_day or samples_per_node")
	}
	urls := []string{t.URL, t.LivenessURL, t.GeoURL}
	if t.TraceURL != "" {
		urls = append(urls, t.TraceURL)
	}
	for _, raw := range urls {
		u, e := url.Parse(raw)
		if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("probe URLs must be absolute HTTP(S) URLs")
		}
	}
	if c.Mihomo.TestGroup == "" {
		return errors.New("missing mihomo.test_group")
	}
	roles := map[string]bool{c.Mihomo.TestGroup: true}
	for _, g := range c.Groups {
		if g.Name == "" || g.Chain == "" || g.MinMbps < 0 || math.IsNaN(g.MinMbps) || math.IsInf(g.MinMbps, 0) {
			return errors.New("invalid managed group")
		}
		for _, name := range []string{g.Name, g.Chain, g.Direct} {
			if name == "" {
				continue
			}
			if roles[name] {
				return fmt.Errorf("duplicate group, chain or test target %q", name)
			}
			roles[name] = true
		}
		if _, err := regexp.Compile(g.Exclude); err != nil {
			return fmt.Errorf("%s exclude_regex: %w", g.Name, err)
		}
		if g.GeoProxy != "" || g.ExpectedGeo != "" {
			if err := localURL(g.GeoProxy, true); err != nil {
				return fmt.Errorf("geo_proxy for %s: %w", g.Name, err)
			}
			if g.ExpectedGeo == "" {
				return fmt.Errorf("missing expected_geo for %s", g.Name)
			}
		}
	}
	return nil
}

func pct(v float64) bool { return v >= 0 && v <= 100 }

func localURL(raw string, proxy bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("expected a loopback URL with an explicit port")
	}
	permitted := u.Scheme == "http" || (!proxy && u.Scheme == "https") || (proxy && (u.Scheme == "socks5" || u.Scheme == "socks5h"))
	if ip := net.ParseIP(u.Hostname()); !permitted || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return errors.New("unsupported scheme or non-loopback address")
	}
	return nil
}
