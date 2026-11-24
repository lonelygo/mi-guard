package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/model"
)

func check(ctx context.Context, r *guard.Runner) error {
	version, err := r.Core.Version(ctx)
	if err != nil {
		return err
	}
	core := r.Cfg.Mihomo
	testGroup, err := r.Core.Proxy(ctx, core.TestGroup)
	if err != nil {
		return err
	}
	if testGroup.Type != "Selector" || len(testGroup.All) == 0 {
		return errors.New("live core is missing the configured test selector or its candidates")
	}
	if err := r.CheckIngress(ctx, core.TestProxy, r.Cfg.Speed.URL, core.TestGroup, testGroup.Now); err != nil {
		return fmt.Errorf("cannot verify live test listener %s for current member %s: %w", core.TestProxy, model.QuoteName(testGroup.Now), err)
	}
	if err := checkGroups(ctx, r, os.Stdout); err != nil {
		return err
	}
	_, err = fmt.Printf("ready: Mihomo %s, %s (%d members)\n", version, testGroup.Name, len(testGroup.All))
	return err
}

func checkGroups(ctx context.Context, r *guard.Runner, out io.Writer) error {
	if len(r.Cfg.Groups) == 0 {
		return nil
	}
	bandwidth, err := readBandwidth(ctx, r)
	if err != nil {
		return err
	}
	probes := make(map[string]memberProbe)
	for _, g := range r.Cfg.Groups {
		nodes, err := checkManagedGroup(ctx, r, g)
		if err != nil {
			return err
		}
		if err = probeMembers(ctx, r, nodes, probes); err != nil {
			return err
		}
		primary, tenure, err := r.ObservePrimary(ctx, g.Name)
		if err != nil {
			return err
		}
		if primary.Now != "" {
			if err := probeMembers(ctx, r, []string{primary.Now}, probes); err != nil {
				return err
			}
		}
		if err = reportLiveGroup(out, g, primary.Now, nodes, probes, bandwidth[primary.Now], tenure, time.Now(), r.Cfg.Selection.MaxAge); err != nil {
			return err
		}
	}
	return nil
}
