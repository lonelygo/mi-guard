package history

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SelectionTenure tracks a primary selector by member name. Since is its
// first observation in this run of the same member, or a confirmed switch.
// LastSeen bounds the span even if the next observation discovers a change.
type SelectionTenure struct {
	Node          string                   `json:"node"`
	Since         time.Time                `json:"since"`
	LastSeen      time.Time                `json:"last_seen"`
	GuardSelected bool                     `json:"guard_selected,omitempty"`
	Longest       map[string]time.Duration `json:"longest_observed_ns,omitempty"`
}

func validateSelections(selections map[string]SelectionTenure) error {
	for group, s := range selections {
		if group == "" || s.Node == "" || s.Since.IsZero() || s.LastSeen.Before(s.Since) {
			return fmt.Errorf("malformed selection history for %q", group)
		}
		for node, span := range s.Longest {
			if node == "" || span < 0 {
				return fmt.Errorf("invalid longest selection span for %q in %q", node, group)
			}
		}
	}
	return nil
}

// v0.1.x daemons strictly decode the bandwidth ledger. A separate file lets
// an upgraded CLI record tenure while an older daemon still accounts traffic.
func (s *Store) readSelections() (map[string]SelectionTenure, error) {
	var selections map[string]SelectionTenure
	if _, err := readHistoryJSON(s.file+".selections.json", &selections); err != nil {
		return nil, err
	}
	if err := validateSelections(selections); err != nil {
		return nil, err
	}
	return selections, nil
}

// RecordSelection records the time of a stable read or a confirmed change.
// A keep is an observation (switched=false). A confirmed change starts a new
// span even if the store missed an intervening manual choice. Late reads
// return the newer durable state. At equal times a confirmed change wins.
func (s *Store) RecordSelection(ctx context.Context, group, node string, at time.Time, switched bool) (SelectionTenure, error) {
	if group == "" || node == "" || at.IsZero() {
		return SelectionTenure{}, errors.New("selection observation needs a group, member and time")
	}
	at = at.UTC()
	var result SelectionTenure
	err := s.WithLock(ctx, "state", func() error {
		selections, err := s.readSelections()
		if err != nil {
			return err
		}
		current := selections[group]
		confirmedAt := current.GuardSelected && current.Since.Equal(at)
		if current.Node == "" || at.After(current.LastSeen) || at.Equal(current.LastSeen) && switched && !confirmedAt {
			if switched || current.Node != node {
				current.Node = node
				current.Since = at
				current.GuardSelected = switched
			}
			current.LastSeen = at
			if span := at.Sub(current.Since); span > current.Longest[node] {
				if current.Longest == nil {
					current.Longest = make(map[string]time.Duration)
				}
				current.Longest[node] = span
			}
			if selections == nil {
				selections = make(map[string]SelectionTenure)
			}
			selections[group] = current
			if err := s.writeHistoryJSON(s.file+".selections.json", selections); err != nil {
				return err
			}
		}
		result = current
		return nil
	})
	return result, err
}
