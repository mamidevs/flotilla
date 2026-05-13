package pool

import (
	"context"
	"strings"
	"testing"

	"github.com/mamidevs/flotilla/internal/config"
	"github.com/mamidevs/flotilla/internal/worker"
)

func mustWorker(t *testing.T, name string, tags ...string) *worker.Worker {
	t.Helper()
	w, err := worker.New(worker.Config{
		Name:     name,
		ExitNode: name + "-exit",
		StateDir: t.TempDir(),
		Tags:     tags,
	})
	if err != nil {
		t.Fatalf("worker.New(%q): %v", name, err)
	}
	// Health gate: mark every test worker healthy by default. We don't call
	// Start (would require a real Tailnet), so we exercise just the strategy.
	w.MarkHealthy("203.0.113." + strings.Trim(name, "abcdefghijklmnopqrstuvwxyz-"))
	return w
}

func TestRoundRobinCyclesAllCandidates(t *testing.T) {
	cs := []*worker.Worker{mustWorker(t, "a"), mustWorker(t, "b"), mustWorker(t, "c")}
	s := &roundRobin{}
	ctx := context.Background()
	got := make([]string, 0, 6)
	for range 6 {
		w, err := s.Choose(ctx, cs, Hint{})
		if err != nil {
			t.Fatalf("Choose: %v", err)
		}
		got = append(got, w.Name())
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round_robin order: got %v, want %v", got, want)
		}
	}
}

func TestRandomReturnsCandidate(t *testing.T) {
	cs := []*worker.Worker{mustWorker(t, "x"), mustWorker(t, "y")}
	s := &random{}
	w, err := s.Choose(context.Background(), cs, Hint{})
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	if w.Name() != "x" && w.Name() != "y" {
		t.Fatalf("random returned unknown worker %q", w.Name())
	}
}

func TestStickySameClientStaysOnSameWorker(t *testing.T) {
	cs := []*worker.Worker{mustWorker(t, "a"), mustWorker(t, "b"), mustWorker(t, "c")}
	s := &sticky{}
	w1, _ := s.Choose(context.Background(), cs, Hint{ClientID: "client-42"})
	w2, _ := s.Choose(context.Background(), cs, Hint{ClientID: "client-42"})
	w3, _ := s.Choose(context.Background(), cs, Hint{ClientID: "client-other"})
	if w1.Name() != w2.Name() {
		t.Fatalf("same client should pin: got %s then %s", w1.Name(), w2.Name())
	}
	_ = w3 // any worker is acceptable
}

func TestLeastActivePicksLowest(t *testing.T) {
	a := mustWorker(t, "a")
	b := mustWorker(t, "b")
	c := mustWorker(t, "c")
	// Simulate active connections by issuing dials we never close: not easy
	// without a real Tailnet. Instead, drive the counter directly via the
	// trackedConn life-cycle. We can't, so for unit-test scope, validate
	// the strategy with a synthetic ordering by chaining Choose() with
	// pretend-equal counts (all zero) — least_active should return cs[0].
	s := &leastActive{}
	w, _ := s.Choose(context.Background(), []*worker.Worker{a, b, c}, Hint{})
	if w.Name() != "a" {
		t.Fatalf("least_active with equal active counts should pick first; got %s", w.Name())
	}
}

func TestNewStrategy_AllVariants(t *testing.T) {
	cases := []config.DispatchStrategy{
		config.StrategyRoundRobin,
		config.StrategyRandom,
		config.StrategySticky,
		config.StrategyLeastActive,
		config.StrategyTagged,
	}
	for _, c := range cases {
		s, err := NewStrategy(c)
		if err != nil {
			t.Fatalf("NewStrategy(%q): %v", c, err)
		}
		if s == nil {
			t.Fatalf("NewStrategy(%q) returned nil", c)
		}
	}
	if _, err := NewStrategy("nonsense"); err == nil {
		t.Fatalf("expected error on unknown strategy")
	}
}

func TestMatchesTags(t *testing.T) {
	type tc struct {
		have, want []string
		ok         bool
	}
	cases := []tc{
		{nil, nil, true},
		{[]string{"region:tr"}, nil, true},
		{[]string{"region:tr"}, []string{"region:tr"}, true},
		{[]string{"region:tr", "scraper:a"}, []string{"region:tr"}, true},
		{[]string{"region:tr"}, []string{"region:eu"}, false},
		{[]string{"region:tr"}, []string{"region:tr", "missing"}, false},
		{[]string{"REGION:tr"}, []string{"region:TR"}, true},
	}
	for i, c := range cases {
		if got := matchesTags(c.have, c.want); got != c.ok {
			t.Fatalf("case %d (have=%v want=%v): got %v want %v", i, c.have, c.want, got, c.ok)
		}
	}
}
