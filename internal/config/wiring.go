package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v2"
)

// Read the same local configuration the operator supplies to the serving core.
// Decode routing metadata only; node credentials never enter reports.
type CoreFile struct {
	Secret    string `yaml:"secret"`
	Listeners []struct {
		Name   string `yaml:"name"`
		Type   string `yaml:"type"`
		Listen string `yaml:"listen"`
		Port   string `yaml:"port"`
		Proxy  string `yaml:"proxy"`
	} `yaml:"listeners"`
	Groups []struct {
		Name    string   `yaml:"name"`
		Type    string   `yaml:"type"`
		Proxies []string `yaml:"proxies"`
	} `yaml:"proxy-groups"`
	Proxies []struct {
		Name   string `yaml:"name"`
		Dialer string `yaml:"dialer-proxy"`
	} `yaml:"proxies"`
	Rules     []string            `yaml:"rules"`
	SubRules  map[string][]string `yaml:"sub-rules"`
	Providers map[string]struct {
		Override struct {
			Dialer string `yaml:"dialer-proxy"`
		} `yaml:"override"`
	} `yaml:"proxy-providers"`
}

func ReadCoreFile(path string) (*CoreFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f CoreFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("cannot decode routing metadata in %s", path)
	}
	return &f, nil
}

func (f *CoreFile) checkListener(addr, target string) error {
	u, err := url.Parse(addr)
	if err != nil {
		return err
	}
	for _, l := range f.Listeners {
		if l.Proxy != target || l.Port != u.Port() {
			continue
		}
		ip := net.ParseIP(l.Listen)
		if l.Listen != "localhost" && (ip == nil || !ip.IsLoopback()) {
			continue
		}
		if l.Listen != u.Hostname() && l.Listen != "localhost" && u.Hostname() != "localhost" {
			continue
		}
		if l.Type == "mixed" || (l.Type == "http" && u.Scheme == "http") || (l.Type == "socks" && strings.HasPrefix(u.Scheme, "socks5")) {
			return nil
		}
	}
	return fmt.Errorf("need a loopback listener %s bound to %q in the serving Mihomo config (see deploy/mihomo-overlay.yaml)", addr, target)
}
func (f *CoreFile) CheckBench(core Core) error {
	if err := f.checkListener(core.TestProxy, core.TestGroup); err != nil {
		return err
	}
	uses := 0
	for _, l := range f.Listeners {
		if l.Proxy == core.TestGroup {
			uses++
		}
	}
	if uses != 1 {
		return fmt.Errorf("expected exactly one dedicated listener for %q, found %d", core.TestGroup, uses)
	}
	found := false
	for _, g := range f.Groups {
		if g.Name == core.TestGroup && g.Type == "select" {
			found = true
		}
		for _, n := range g.Proxies {
			if n == core.TestGroup {
				return fmt.Errorf("bench selector is also used by %q", g.Name)
			}
		}
	}
	for _, p := range f.Proxies {
		if p.Dialer == core.TestGroup {
			return fmt.Errorf("bench selector is a production dialer for %q", p.Name)
		}
	}
	for name, p := range f.Providers {
		if p.Override.Dialer == core.TestGroup {
			return fmt.Errorf("bench selector is a dialer for provider %q", name)
		}
	}
	rules := append([]string(nil), f.Rules...)
	for _, subrules := range f.SubRules {
		rules = append(rules, subrules...)
	}
	for _, rule := range rules {
		for _, field := range strings.Split(rule, ",") {
			if strings.TrimSpace(field) == core.TestGroup {
				return errors.New("bench selector is referenced by a traffic rule")
			}
		}
	}
	if !found {
		return fmt.Errorf("missing dedicated select group %q", core.TestGroup)
	}
	return nil
}
func (f *CoreFile) CheckManaged(gs []Group) error {
	for _, g := range gs {
		if err := f.checkManagedTarget(g); err != nil {
			return err
		}
		if g.GeoProxy != "" {
			if err := f.checkListener(g.GeoProxy, g.ProbeTarget()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *CoreFile) checkManagedTarget(g Group) error {
	if g.Mode == GroupModeSelector {
		for _, group := range f.Groups {
			if group.Name == g.Name && group.Type == "select" {
				return nil
			}
		}
		return fmt.Errorf("group %q must be a select group in the serving config", g.Name)
	}
	for _, proxy := range f.Proxies {
		if proxy.Name == g.Chain && proxy.Dialer == g.Name {
			return nil
		}
	}
	return fmt.Errorf("%q must use dialer-proxy %q in the serving config", g.Chain, g.Name)
}
