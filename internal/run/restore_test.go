package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/karba-studio/outline-backup/internal/cfg"
)

// snapshotDir builds a fake snapshot containing the named dumps.
func snapshotDir(t *testing.T, dumps ...string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, DirDatabase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range dumps {
		if err := os.WriteFile(filepath.Join(dir, d+".dump"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// This is the regression test for a real incident: a restore aimed at scratch
// databases wrote to the live ones instead, because the target name came from
// the dump's file name rather than from the configuration.
func TestPlanDatabasesIgnoresSourceNames(t *testing.T) {
	root := snapshotDir(t, RoleInstance, RoleKeycloak)
	in := &cfg.Instance{
		Name:             "business",
		Database:         "outline_business_restoretest",
		KeycloakDatabase: "keycloak_restoretest",
	}

	plan, err := planDatabases(root, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(plan), plan)
	}

	got := map[string]string{}
	for _, p := range plan {
		if p.skipReason != "" {
			t.Errorf("%s was skipped: %s", p.role, p.skipReason)
		}
		got[p.role] = p.target
	}
	if got[RoleInstance] != "outline_business_restoretest" {
		t.Errorf("instance dump targets %q, want the configured scratch database",
			got[RoleInstance])
	}
	if got[RoleKeycloak] != "keycloak_restoretest" {
		t.Errorf("keycloak dump targets %q, want the configured scratch database",
			got[RoleKeycloak])
	}
	for _, target := range got {
		if target == "outline_business" || target == "keycloak" {
			t.Fatalf("plan targets the live database %q; this is the bug this test exists for", target)
		}
	}
}

// Without a configured target there is nowhere safe to put Keycloak, so it must
// be skipped rather than written to whatever the source host called it.
func TestPlanDatabasesSkipsKeycloakWithoutTarget(t *testing.T) {
	root := snapshotDir(t, RoleInstance, RoleKeycloak)
	in := &cfg.Instance{Name: "business", Database: "scratch_db"}

	plan, err := planDatabases(root, in)
	if err != nil {
		t.Fatal(err)
	}
	var sawSkip bool
	for _, p := range plan {
		if p.role == RoleKeycloak {
			if p.skipReason == "" {
				t.Errorf("keycloak would be restored into %q with no target configured", p.target)
			}
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Error("keycloak dump was dropped from the plan entirely; it should be reported as skipped")
	}
}

// Snapshots taken before dumps were named by role carry the source database
// name. They must still restore, and still into the configured target.
func TestPlanDatabasesReadsLegacyLayout(t *testing.T) {
	root := snapshotDir(t, "outline_business", "keycloak")
	in := &cfg.Instance{
		Name:             "business",
		Database:         "outline_business_restoretest",
		KeycloakDatabase: "keycloak_restoretest",
	}

	plan, err := planDatabases(root, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(plan), plan)
	}
	for _, p := range plan {
		switch p.role {
		case RoleInstance:
			if p.target != "outline_business_restoretest" {
				t.Errorf("legacy instance dump targets %q", p.target)
			}
		case RoleKeycloak:
			if p.target != "keycloak_restoretest" {
				t.Errorf("legacy keycloak dump targets %q", p.target)
			}
		}
	}
}

func TestPlanDatabasesRejectsEmptySnapshot(t *testing.T) {
	root := snapshotDir(t)
	if _, err := planDatabases(root, &cfg.Instance{Name: "x", Database: "d"}); err == nil {
		t.Error("a snapshot with no dumps was accepted")
	}
}
