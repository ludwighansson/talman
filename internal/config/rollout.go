package config

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// Rollout is the order a roll-out takes through the cluster.
type Rollout struct {
	// Soak is how long to wait after each wave before the next, as a Go
	// duration. Empty is no wait.
	Soak string `yaml:"soak,omitempty"`
	// Waves are rolled out in order. A node belongs to the first wave that
	// names its role or one of its groups; nodes no wave names go last.
	Waves []Wave `yaml:"waves"`
}

// Wave is one step of a roll-out: the nodes in any of its groups.
type Wave struct {
	Groups []string `yaml:"groups"`
	// Pause stops the roll-out after this wave, to be carried on with
	// --from once whoever is watching is satisfied.
	Pause bool `yaml:"pause,omitempty"`
}

// UnmarshalYAML accepts a wave as a group name, a list of them, or the long
// form with groups and pause.
func (w *Wave) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var g string
		if err := value.Decode(&g); err != nil {
			return err
		}

		w.Groups = []string{g}

		return nil
	case yaml.SequenceNode:
		return value.Decode(&w.Groups)
	case yaml.MappingNode:
		// Strict, like the rest of the file: round-tripped through a decoder
		// with KnownFields, which a yaml.Node decode does not offer.
		raw, err := yaml.Marshal(value)
		if err != nil {
			return err
		}

		type plain Wave

		var p plain

		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)

		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("line %d: %w", value.Line, err)
		}

		*w = Wave(p)

		return nil
	default:
		return fmt.Errorf("line %d: a wave is a group name, a list of them, or {groups: [...], pause: true}",
			value.Line)
	}
}

// Name is how a wave is spoken of: its groups, comma-separated.
func (w Wave) Name() string { return strings.Join(w.Groups, ",") }

// SoakDuration is Soak parsed; Validate has already refused one that does not
// parse.
func (r *Rollout) SoakDuration() time.Duration {
	if r == nil || r.Soak == "" {
		return 0
	}

	d, _ := time.ParseDuration(r.Soak)

	return d
}

// WaveOf is the index of the wave naming group -- "rest" for the nodes no
// wave names -- and whether there is one.
func (r *Rollout) WaveOf(group string) (int, bool) {
	if r == nil {
		return 0, false
	}

	for i, w := range r.Waves {
		if slices.Contains(w.Groups, group) {
			return i, true
		}
	}

	if group == RestWave {
		return len(r.Waves), true
	}

	return 0, false
}

// Staged is a roll-out's nodes laid out in its waves.
type Staged struct {
	// Wave is the rollout wave these nodes belong to, or nil for the nodes
	// no wave names, which go last.
	Wave  *Wave
	Index int
	Nodes []*Node
}

// RestWave names the nodes no wave names, which go last.
const RestWave = "rest"

// Name is the wave's name, or "rest".
func (s Staged) Name() string {
	if s.Wave == nil {
		return RestWave
	}

	return s.Wave.Name()
}

// Stages lays targets out in the rollout's waves, config order within each,
// leaving out waves none of them are in. Without a rollout it is one stage
// holding every target.
func (c *Config) Stages(targets []*Node) []Staged {
	if c.Rollout == nil || len(c.Rollout.Waves) == 0 {
		return []Staged{{Index: 0, Nodes: targets}}
	}

	waves := c.Rollout.Waves
	stages := make([]Staged, len(waves)+1)

	for i := range waves {
		stages[i] = Staged{Wave: &waves[i], Index: i}
	}

	stages[len(waves)] = Staged{Index: len(waves)}

	for _, n := range targets {
		stages[c.waveIndex(n)].Nodes = append(stages[c.waveIndex(n)].Nodes, n)
	}

	out := stages[:0]

	for _, s := range stages {
		if len(s.Nodes) > 0 {
			out = append(out, s)
		}
	}

	return out
}

// waveIndex is the first wave naming the node's role or one of its groups,
// or one past the last when none does.
func (c *Config) waveIndex(n *Node) int {
	for i, w := range c.Rollout.Waves {
		for _, g := range w.Groups {
			if g == string(n.Role) || slices.Contains(n.Groups, g) {
				return i
			}
		}
	}

	return len(c.Rollout.Waves)
}

func (c *Config) validateRollout(add func(string, ...any)) {
	r := c.Rollout
	if r == nil {
		return
	}

	if r.Soak != "" {
		if d, err := time.ParseDuration(r.Soak); err != nil || d < 0 {
			add("rollout.soak %q is not a duration, e.g. 10m", r.Soak)
		}
	}

	if len(r.Waves) == 0 {
		add("rollout.waves is empty: list the groups to roll out, in order")
	}

	declared := c.DeclaredGroups()
	seen := map[string]int{}

	for i, w := range r.Waves {
		where := fmt.Sprintf("rollout.waves[%d]", i)

		if len(w.Groups) == 0 {
			add("%s names no group", where)
		}

		for _, g := range w.Groups {
			switch {
			case g == GroupAll:
				add("%s: %q is every node, which leaves nothing for the other waves; "+
					"nodes no wave names go last anyway", where, g)
			case g != GroupControlPlane && g != GroupWorker && !declared[g]:
				add("%s: no node declares the group %q", where, g)
			}

			if prev, dup := seen[g]; dup {
				add("%s: group %q is already rollout.waves[%d]'s", where, g, prev)
			}

			seen[g] = i
		}
	}
}
