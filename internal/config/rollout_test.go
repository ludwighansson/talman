package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const rolloutNodes = `nodes:
  - hostname: c1
    ipAddress: 10.0.0.1
    role: controlplane
  - hostname: b1
    ipAddress: 10.0.0.2
    role: worker
    groups: [blue]
  - hostname: g1
    ipAddress: 10.0.0.3
    role: worker
    groups: [green, blue]
  - hostname: p1
    ipAddress: 10.0.0.4
    role: worker
    groups: [pink]
  - hostname: x1
    ipAddress: 10.0.0.5
    role: worker
`

func TestRolloutStages(t *testing.T) {
	cfg, err := Load(write(t, validBase+`rollout:
  soak: 10m
  waves:
    - controlplane
    - blue
    - [green, pink]
    - red
`+rolloutNodes+`  - hostname: r1
    ipAddress: 10.0.0.6
    role: worker
    groups: [red]
`))
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.Rollout.SoakDuration(); got != 10*time.Minute {
		t.Errorf("soak = %s", got)
	}

	targets := make([]*Node, 0, len(cfg.Nodes))
	for i := range cfg.Nodes {
		targets = append(targets, &cfg.Nodes[i])
	}

	var got []string

	for _, s := range cfg.Stages(targets) {
		names := make([]string, 0, len(s.Nodes))
		for _, n := range s.Nodes {
			names = append(names, n.Hostname)
		}

		got = append(got, fmt.Sprintf("%d/%d %s=%s", s.Number, s.Of, s.Name(), strings.Join(names, " ")))
	}

	// g1 is in blue and green, and goes with the first wave naming either;
	// x1 is in no wave and goes last.
	want := "1/5 controlplane=c1 | 2/5 blue=b1 g1 | 3/5 green,pink=p1 | 4/5 red=r1 | 5/5 rest=x1"
	if strings.Join(got, " | ") != want {
		t.Errorf("stages:\n  got  %s\n  want %s", strings.Join(got, " | "), want)
	}

	// Numbered among the waves the targets are in, not the config's: a wave
	// none of them is in is not one a run goes through.
	stages := cfg.Stages([]*Node{&cfg.Nodes[3], &cfg.Nodes[4]})
	if len(stages) != 2 || stages[0].Number != 1 || stages[1].Number != 2 || stages[1].Of != 2 {
		t.Errorf("stages of p1 and x1 = %+v, want green,pink 1/2 and rest 2/2", stages)
	}
}

func TestRolloutWithoutWavesIsConfigOrder(t *testing.T) {
	cfg, err := Load(write(t, validBase+rolloutNodes))
	if err != nil {
		t.Fatal(err)
	}

	targets := []*Node{&cfg.Nodes[2], &cfg.Nodes[0]}

	stages := cfg.Stages(targets)
	if len(stages) != 1 || len(stages[0].Nodes) != 2 || stages[0].Nodes[0] != targets[0] {
		t.Errorf("without a rollout, one stage holding the targets as given; got %+v", stages)
	}
}

func TestRolloutValidation(t *testing.T) {
	for name, tt := range map[string]struct{ rollout, want string }{
		"unknown group":  {"waves: [blue, purple]", `no node declares the group "purple"`},
		"all":            {"waves: [all]", `"all" is every node`},
		"group twice":    {"waves: [blue, [green, blue]]", `already rollout.waves[0]'s`},
		"bad soak":       {"soak: ten minutes\n  waves: [blue]", "rollout.soak"},
		"no waves":       {"waves: []", "rollout.waves is empty"},
		"a mapping":      {"waves: [{groups: [blue]}]", "a wave is a group name, or a list of them"},
		"rest as a wave": {"waves: [blue, rest]", `"rest" is the nodes no wave names`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, validBase+"rollout:\n  "+tt.rollout+"\n"+rolloutNodes))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// "rest" names the nodes no wave names, so no node may claim it as a group.
func TestRestIsReserved(t *testing.T) {
	_, err := Load(write(t, validBase+`nodes:
  - hostname: c1
    ipAddress: 10.0.0.1
    role: controlplane
    groups: [rest]
`))
	if err == nil || !strings.Contains(err.Error(), `group "rest" is reserved`) {
		t.Errorf("Load() = %v", err)
	}
}
