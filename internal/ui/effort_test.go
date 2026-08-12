package ui

import (
	"strings"
	"testing"

	"tapioca/internal/agent"
)

// The levels are a scale, so their order is part of the meaning — a map would
// have rendered them in whatever order Go chose that run.
func TestEffortLevelsAreOrderedAscending(t *testing.T) {
	if len(effortLevels) < 2 {
		t.Fatal("expected several levels")
	}
	for i := 1; i < len(effortLevels); i++ {
		if effortLevels[i].Budget <= effortLevels[i-1].Budget {
			t.Errorf("%s (%d) does not come after %s (%d)",
				effortLevels[i].Name, effortLevels[i].Budget,
				effortLevels[i-1].Name, effortLevels[i-1].Budget)
		}
	}
	if effortLevels[0].Name != "off" || effortLevels[0].Budget != 0 {
		t.Errorf("the first level is %+v, want off/0", effortLevels[0])
	}
}

func TestEffortBudgetLookup(t *testing.T) {
	for _, l := range effortLevels {
		if got, ok := effortBudget(l.Name); !ok || got != l.Budget {
			t.Errorf("effortBudget(%q) = %d,%v", l.Name, got, ok)
		}
	}
	// Typed by a person, so it has to survive their spacing and capitals.
	if got, ok := effortBudget("  HIGH "); !ok || got != 16384 {
		t.Errorf(`effortBudget("  HIGH ") = %d,%v`, got, ok)
	}
	// And it must reject rather than silently pick something.
	for _, bad := range []string{"max", "", "medium-ish", "9000"} {
		if _, ok := effortBudget(bad); ok {
			t.Errorf("effortBudget(%q) was accepted", bad)
		}
	}
}

// effortName reads a stored number back into a level, including a budget set
// by hand in the config that lands between two of them.
func TestEffortNameFromBudget(t *testing.T) {
	cases := []struct {
		thinking bool
		budget   int
		want     string
	}{
		{false, 16384, "off"}, // thinking off wins whatever the budget says
		{true, 0, "low"},      // nonsense budget with thinking on: the lowest real level
		{true, 1024, "low"},
		{true, 2000, "low"}, // between low and medium
		{true, 4096, "medium"},
		{true, 16384, "high"},
		{true, 99999, "high"},
	}
	for _, c := range cases {
		a := &agent.Agent{Thinking: c.thinking, ThinkingBudget: c.budget}
		if got := effortName(a); got != c.want {
			t.Errorf("thinking=%v budget=%d: got %q, want %q", c.thinking, c.budget, got, c.want)
		}
	}
}

// The usage message has to list what is actually accepted, or it sends people
// to a level that does not exist.
func TestEffortNamesMatchTheLevels(t *testing.T) {
	names := effortNames()
	for _, l := range effortLevels {
		if !strings.Contains(names, l.Name) {
			t.Errorf("usage text %q omits %q", names, l.Name)
		}
	}
}
