package pool

import (
	"context"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"sync/atomic"

	"github.com/mamidevs/flotilla/internal/config"
	"github.com/mamidevs/flotilla/internal/worker"
)

// Strategy decides which Worker handles a given request.
type Strategy interface {
	Choose(ctx context.Context, candidates []*worker.Worker, hint Hint) (*worker.Worker, error)
	Name() config.DispatchStrategy
}

// NewStrategy returns a Strategy for the configured dispatch mode.
func NewStrategy(s config.DispatchStrategy) (Strategy, error) {
	switch s {
	case config.StrategyRoundRobin:
		return &roundRobin{}, nil
	case config.StrategyRandom:
		return &random{}, nil
	case config.StrategySticky:
		return &sticky{}, nil
	case config.StrategyLeastActive:
		return &leastActive{}, nil
	case config.StrategyTagged:
		// Tagged falls back to round-robin once the pool has applied the tag filter.
		return &roundRobin{taggedAlias: true}, nil
	default:
		return nil, errors.New("unknown strategy: " + string(s))
	}
}

type roundRobin struct {
	idx         atomic.Uint64
	taggedAlias bool
}

func (r *roundRobin) Choose(_ context.Context, cs []*worker.Worker, _ Hint) (*worker.Worker, error) {
	if len(cs) == 0 {
		return nil, ErrNoHealthyWorker
	}
	i := r.idx.Add(1) - 1
	return cs[int(i%uint64(len(cs)))], nil //nolint:gosec
}

func (r *roundRobin) Name() config.DispatchStrategy {
	if r.taggedAlias {
		return config.StrategyTagged
	}
	return config.StrategyRoundRobin
}

type random struct{}

func (r *random) Choose(_ context.Context, cs []*worker.Worker, _ Hint) (*worker.Worker, error) {
	if len(cs) == 0 {
		return nil, ErrNoHealthyWorker
	}
	// #nosec G404 -- load-balancing pick, not security-sensitive.
	return cs[rand.IntN(len(cs))], nil
}

func (r *random) Name() config.DispatchStrategy { return config.StrategyRandom }

type sticky struct{}

func (s *sticky) Choose(_ context.Context, cs []*worker.Worker, h Hint) (*worker.Worker, error) {
	if len(cs) == 0 {
		return nil, ErrNoHealthyWorker
	}
	if h.ClientID == "" {
		// No stickiness possible; fall through to a stable bucket-0 pick.
		return cs[0], nil
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(h.ClientID))
	idx := int(hash.Sum32()) % len(cs)
	if idx < 0 {
		idx = -idx
	}
	return cs[idx], nil
}

func (s *sticky) Name() config.DispatchStrategy { return config.StrategySticky }

type leastActive struct{}

func (l *leastActive) Choose(_ context.Context, cs []*worker.Worker, _ Hint) (*worker.Worker, error) {
	if len(cs) == 0 {
		return nil, ErrNoHealthyWorker
	}
	best := cs[0]
	bestN := best.ActiveConns()
	for _, w := range cs[1:] {
		if n := w.ActiveConns(); n < bestN {
			best, bestN = w, n
		}
	}
	return best, nil
}

func (l *leastActive) Name() config.DispatchStrategy { return config.StrategyLeastActive }
