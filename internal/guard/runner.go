// Package guard measures candidates through a dedicated selector in the running core.
package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/probe"
)

type Runner struct {
	Cfg    config.Config
	Core   *mihomo.Client
	Store  *history.Store
	Notify func(kind, title, body string)
	Log    *slog.Logger

	geoCheck func(context.Context, config.Group) (probe.GeoResult, error)
}

// A core can be shared by guard configs with different history files. Use a
// per-user resource lock for its mutable selectors, independently of accounting.
func withSelectorLock(ctx context.Context, name string, fn func() error) error {
	base := filepath.Join(os.TempDir(), fmt.Sprintf("mi-guard-%d", os.Getuid()), "selectors")
	return history.New(base, 1, 1).WithLock(ctx, name, fn)
}

func (r *Runner) Managed(name string) (config.Group, bool) {
	for _, g := range r.Cfg.Groups {
		if g.Name == name {
			return g, true
		}
	}
	return config.Group{}, false
}
func physical(p model.Proxy) bool {
	if p.All != nil || p.Name == "" {
		return false
	}
	switch p.Type {
	case "Ambiguous", "Direct", "Reject", "RejectDrop", "Pass", "Compatible", "Dns", "DNS", "Selector", "URLTest", "Fallback", "LoadBalance", "Relay":
		return false
	}
	return true
}

// Candidates returns controller names after provider overrides. Only direct
// physical members can be applied to a managed production selector.
func (r *Runner) Candidates(ctx context.Context, group string) ([]string, error) {
	all, err := r.Core.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	g, managed := r.Managed(group)
	target, ok := all[group]
	if !ok {
		return nil, fmt.Errorf("unknown proxy group %q", group)
	}
	if target.All == nil {
		return nil, fmt.Errorf("proxy %q has no candidates", group)
	}
	if managed && target.Type != "Selector" {
		return nil, fmt.Errorf("%q is %s, want Selector", group, target.Type)
	}
	bench, ok := all[r.Cfg.Mihomo.TestGroup]
	if !ok || bench.Type != "Selector" {
		return nil, fmt.Errorf("load the dedicated Mihomo selector %q first", r.Cfg.Mihomo.TestGroup)
	}
	for name, p := range all {
		if p.Dialer == bench.Name || (name != "GLOBAL" && name != bench.Name && slices.Contains(p.All, bench.Name)) {
			return nil, fmt.Errorf("live %q consumes dedicated bench selector %q", name, bench.Name)
		}
	}
	allowed := make(map[string]bool, len(bench.All))
	for _, name := range bench.All {
		allowed[name] = true
	}
	var exclude *regexp.Regexp
	if managed && g.Exclude != "" {
		exclude = regexp.MustCompile(g.Exclude)
	}
	seen := map[string]bool{group: true}
	var names []string
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		p, ok := all[name]
		if !ok {
			return
		}
		if physical(p) {
			if allowed[name] && (exclude == nil || !exclude.MatchString(name)) {
				names = append(names, name)
			}
		} else if !managed {
			for _, member := range p.All {
				visit(member)
			}
		}
	}
	for _, member := range target.All {
		visit(member)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no eligible physical members of %q in %q", group, bench.Name)
	}
	return names, nil
}

func (r *Runner) Measure(ctx context.Context, node, kind string, limit int64) (s model.Sample, err error) {
	err = withSelectorLock(ctx, "measure", func() (err error) {
		p, err := r.Core.Proxy(ctx, node)
		if err != nil {
			return err
		}
		if !physical(p) {
			return fmt.Errorf("%q is not a physical proxy", node)
		}
		group := r.Cfg.Mihomo.TestGroup
		before, err := r.Core.Proxy(ctx, group)
		if err != nil {
			return err
		}
		if before.Type != "Selector" || !slices.Contains(before.All, node) {
			return fmt.Errorf("%q is not an available member of test selector %q", node, group)
		}
		if before.Now != node {
			defer func() {
				restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer cancel()
				current, e := r.Core.Proxy(restore, group)
				if e == nil && current.Now == node {
					e = r.Core.Select(restore, group, before.Now)
				}
				if e != nil {
					err = errors.Join(err, fmt.Errorf("restore test selector: %w", e))
				}
			}()
			if err = r.Core.Select(ctx, group, node); err != nil {
				return err
			}
		}
		selected, err := r.Core.Proxy(ctx, group)
		if err != nil {
			return err
		}
		if selected.Now != node {
			return errors.New("test selector changed before measurement")
		}
		res, err := r.Store.Reserve(ctx, limit, r.Cfg.Speed.MonthlyBytes)
		if err != nil {
			return err
		}
		attributed := false
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if attributed {
				err = errors.Join(err, r.Store.Finish(closeCtx, res, s))
			} else {
				err = errors.Join(err, r.Store.Discard(closeCtx, res, s.Bytes))
				s = model.Sample{}
			}
		}()
		if res.Bytes < limit {
			return fmt.Errorf("%w: need %d bytes, %d available", history.ErrBudget, limit, res.Bytes)
		}
		s = model.Sample{At: time.Now().UTC(), Scope: "front", Kind: kind, Node: node, Provider: p.Provider, Attempts: 1}
		if p.Dialer != "" {
			s.Scope = "chain:" + node
		}
		s.LatencyMS, err = r.Core.Delay(ctx, node, r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout)
		if err != nil {
			if !mihomo.ProbeUnavailable(err) {
				return err
			}
			s.Failures = 1
			s.Error = fmt.Sprintf("204 probe failed, download skipped: %v", err)
			attributed = true
			return nil
		}
		s.Attempts++
		d, downloadErr := probe.DownloadBatch(ctx, r.Cfg.Mihomo.TestProxy, r.Cfg.Speed.URL, res.Bytes, r.Cfg.Speed.Timeout, r.Cfg.Speed.Streams, r.route(group, node))
		s.Bytes, s.ElapsedMS, s.Mbps = d.Bytes, d.ElapsedMS, d.Mbps
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(downloadErr, probe.ErrRoute) {
			return downloadErr
		}
		if downloadErr != nil {
			s.Failures++
			s.Error = downloadErr.Error()
			s.Mbps = 0
		}
		selected, err = r.Core.Proxy(ctx, group)
		if err != nil || selected.Now != node {
			s.Mbps = 0
			s.Error = "could not verify test selector after download"
			return errors.Join(errors.New(s.Error), err)
		}
		attributed = true
		return nil
	})
	return s, err
}

func (r *Runner) Scan(ctx context.Context, nodes []string, kind string, limit int64) ([]model.Sample, error) {
	out := make([]model.Sample, 0, len(nodes))
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n] {
			continue
		}
		seen[n] = true
		s, err := r.Measure(ctx, n, kind, limit)
		if s.Node != "" {
			out = append(out, s)
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// checkGeo validates the selected WARP exit using a fresh local proxy connection.
func (r *Runner) checkGeo(ctx context.Context, g config.Group) (probe.GeoResult, error) {
	if r.geoCheck != nil {
		return r.geoCheck(ctx, g)
	}
	return probe.GeoChecked(ctx, g.GeoProxy, r.Cfg.Speed.GeoURL, r.Cfg.Speed.TraceURL, g.ExpectedGeo, r.Cfg.Speed.Timeout, r.route(g.ProbeTarget()))
}

func (r *Runner) route(names ...string) probe.RouteCheck {
	return func(ctx context.Context, conn net.Conn) error { return r.Core.VerifyRoute(ctx, conn, names...) }
}

// CheckIngress opens a tunnel and inspects the live route without requesting a download.
func (r *Runner) CheckIngress(ctx context.Context, proxyURL, target string, names ...string) error {
	return probe.CheckRoute(ctx, proxyURL, target, r.Cfg.Speed.Timeout, r.route(names...))
}
func (r *Runner) ExitStatus(ctx context.Context, g config.Group) (probe.GeoResult, error) {
	return r.checkGeo(ctx, g)
}
