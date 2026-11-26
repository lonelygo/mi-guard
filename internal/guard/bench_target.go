package guard

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/policy"
)

// BenchTarget identifies the live group whose members are measured. A chain
// uses its dialer group for selection and its own name for route validation.
type BenchTarget struct {
	Group   config.Group
	Type    string
	Current string
	// DecisionTrigger overrides inference when a workflow reuses historical
	// samples. Hard recovery keeps each sample's original measurement kind.
	DecisionTrigger string
	// Evidence keeps all measurements behind a decision, including failures
	// that policy ranking correctly omits from the selectable list.
	Evidence []model.Sample
	// History is passed only for explaining an already computed policy ranking.
	History []model.Sample
	// LatencyGate is captured before the first measurement of this group.
	LatencyGate policy.LatencyGate
}

func (r *Runner) GroupLatencyGate(group string) (policy.LatencyGate, error) {
	if group == "" || group == r.Cfg.Mihomo.TestGroup {
		return policy.LatencyGate{}, nil
	}
	gate := policy.LatencyGate{Factor: r.Cfg.Selection.LatencyOutlierFactor}
	if gate.Factor == 0 || r.Store == nil {
		return gate, nil
	}
	baseline, err := r.Store.LatencyBaseline(group, r.Cfg.LatencyProbeKey())
	if err != nil {
		return gate, fmt.Errorf("read group latency history for %s: %w", model.QuoteName(group), err)
	}
	gate.MeanMS, gate.Samples = baseline.MeanMS, baseline.Samples
	return gate, nil
}

func (r *Runner) RecordGroupLatencies(ctx context.Context, group string, measured []model.Sample) error {
	if group == "" || group == r.Cfg.Mihomo.TestGroup || r.Store == nil {
		return nil
	}
	return r.Store.RecordLatencyRound(ctx, group, r.Cfg.LatencyProbeKey(), measured)
}

func (r *Runner) ResolveBenchTarget(ctx context.Context, query string) (BenchTarget, error) {
	if strings.TrimSpace(query) == "" {
		return BenchTarget{}, errors.New("empty group name")
	}
	all, err := r.Core.Inventory(ctx)
	if err != nil {
		return BenchTarget{}, err
	}
	isTarget := func(p model.Proxy) bool {
		return p.All != nil || physical(p) && p.Dialer != "" && all[p.Dialer].All != nil
	}
	name := query
	if !isTarget(all[name]) {
		// A quoted literal uses Unicode simple case folding (including Greek final sigma).
		matchNames := func(text string) ([]string, error) {
			match, err := regexp.Compile("(?i)" + regexp.QuoteMeta(text))
			if err != nil {
				return nil, fmt.Errorf("invalid group query: %w", err)
			}
			var hits []string
			for key, p := range all {
				if isTarget(p) && match.MatchString(key) {
					hits = append(hits, key)
				}
			}
			return hits, nil
		}
		text := strings.TrimSpace(query)
		hits, err := matchNames(text)
		if err != nil {
			return BenchTarget{}, err
		}
		// Flags can look identical while using different regional indicators.
		// Keep literal emoji useful for disambiguation, then try the text.
		plain := strings.TrimLeftFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
		if len(hits) == 0 && plain != "" && plain != text {
			hits, err = matchNames(plain)
			if err != nil {
				return BenchTarget{}, err
			}
		}
		switch len(hits) {
		case 0:
			return BenchTarget{}, fmt.Errorf("no live group or dialer chain matches %s", model.QuoteName(query))
		case 1:
			name = hits[0]
		default:
			slices.Sort(hits)
			for i := range hits {
				hits[i] = model.QuoteName(hits[i])
			}
			return BenchTarget{}, fmt.Errorf("ambiguous group %s: %s", model.QuoteName(query), strings.Join(hits, ", "))
		}
	}

	group := config.Group{Name: name}
	p := all[name]
	if p.All == nil {
		group.Name, group.Chain = p.Dialer, name
		for _, g := range r.Cfg.Groups {
			if g.Chain == name && g.Name != group.Name {
				return BenchTarget{}, fmt.Errorf("live chain %s uses dialer-proxy %s, expected %s", model.QuoteName(name), model.QuoteName(group.Name), model.QuoteName(g.Name))
			}
		}
	}
	front := all[group.Name]
	return BenchTarget{Group: r.benchGroup(group), Type: front.Type, Current: front.Now}, nil
}

func (r *Runner) benchGroup(group config.Group) config.Group {
	chain := group.Chain
	if managed, ok := r.Managed(group.Name); ok {
		if chain == "" || managed.Chain == chain {
			return managed
		}
		group = managed
	}
	if chain != "" {
		group.Chain = chain
		group.Mode = config.GroupModeChain
	} else {
		group.Mode = config.GroupModeSelector
	}
	return group
}
