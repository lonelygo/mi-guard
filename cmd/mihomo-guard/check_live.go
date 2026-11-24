package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/mihomo"
)

type memberProbe struct {
	delay int
	err   error
}

// Probe candidates directly through the controller's URL-test endpoint. This
// never selects a member or starts a bandwidth download.
func probeMembers(ctx context.Context, r *guard.Runner, nodes []string, seen map[string]memberProbe) error {
	var pending []string
	for _, node := range nodes {
		if _, exists := seen[node]; !exists {
			pending = append(pending, node)
		}
	}
	if len(pending) == 0 {
		return ctx.Err()
	}
	results := make([]memberProbe, len(pending))
	jobs := make(chan int, len(pending))
	for i := range pending {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(4, len(pending)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				results[i].delay, results[i].err = r.Core.Delay(ctx, pending[i], r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout)
			}
		}()
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, node := range pending {
		seen[node] = results[i]
	}
	return nil
}

func (p memberProbe) label() string {
	if p.err == nil {
		return fmt.Sprintf("%d ms", p.delay)
	}
	if status, ok := mihomo.ProbeStatus(p.err); ok {
		return fmt.Sprintf("失败 (HTTP %d)", status)
	}
	return "未知（控制器未能确认）"
}
