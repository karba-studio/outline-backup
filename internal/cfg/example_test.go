package cfg

import (
	"path/filepath"
	"testing"
)

// The example config is documentation, and documentation that no longer parses
// is worse than none. Load() uses DisallowUnknownFields, so this also catches a
// field being renamed in the struct without being renamed in the example.
func TestExampleConfigIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.jsonc")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("config.example.jsonc does not parse: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("config.example.jsonc does not validate: %v", err)
	}

	if len(c.Instances) < 2 {
		t.Fatalf("example should show at least two instances, has %d", len(c.Instances))
	}

	// The example exists to demonstrate what the config model can express, so
	// it should show both shapes people ask about: one wiki copied to several
	// places, and several wikis sharing one place.
	multi := false
	for i := range c.Instances {
		if len(c.Instances[i].BackupTo) > 1 {
			multi = true
		}
	}
	if !multi {
		t.Error("no instance in the example has more than one destination")
	}

	users := map[string]int{}
	for i := range c.Instances {
		for _, d := range c.Instances[i].BackupTo {
			users[d]++
		}
	}
	shared := false
	for _, n := range users {
		if n > 1 {
			shared = true
		}
	}
	if !shared {
		t.Error("no destination in the example is shared by two instances")
	}

	// Every referenced destination must resolve to its own repository.
	seen := map[string]string{}
	for i := range c.Instances {
		targets, err := c.Targets(&c.Instances[i])
		if err != nil {
			t.Fatalf("instance %q: %v", c.Instances[i].Name, err)
		}
		for _, tg := range targets {
			if other, dup := seen[tg.Repo]; dup {
				t.Errorf("%s and %s/%s share repository %s",
					other, c.Instances[i].Name, tg.Name, tg.Repo)
			}
			seen[tg.Repo] = c.Instances[i].Name + "/" + tg.Name
		}
	}
}
