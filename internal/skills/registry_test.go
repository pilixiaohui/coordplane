package skills_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"coordplane/internal/skills"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestSkillListAndReadAreTrimmedByTeamConfigBindings(t *testing.T) {
	ctx := context.Background()
	registry := skills.NewRegistry(newTestStore(t))
	if err := registry.RegisterBuiltins(ctx); err != nil {
		t.Fatalf("register builtins: %v", err)
	}
	cfg := teamconfig.Config{
		TeamID:  "default-go-team",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "builder",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Skills:         []string{"coordplane-service"},
			},
			{
				ID:             "coordinator",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Skills:         []string{"coordplane-service", "contract-delegation"},
			},
		},
	}

	builderSkills, err := registry.ListForAgent(ctx, cfg, "builder")
	if err != nil {
		t.Fatalf("list builder skills: %v", err)
	}
	if len(builderSkills) != 1 || builderSkills[0].Name != "coordplane-service" {
		t.Fatalf("builder skills = %+v, want only coordplane-service", builderSkills)
	}
	if _, err := registry.ReadForAgent(ctx, cfg, "builder", "contract-delegation"); err == nil {
		t.Fatal("builder read of unbound contract-delegation returned nil error")
	}

	coordSkill, err := registry.ReadForAgent(ctx, cfg, "coordinator", "contract-delegation")
	if err != nil {
		t.Fatalf("coordinator read contract-delegation: %v", err)
	}
	if coordSkill.Version != 1 || !strings.Contains(coordSkill.Content, "contract.add") {
		t.Fatalf("contract-delegation skill = %+v", coordSkill)
	}
}

func TestDisabledSkillDoesNotAppearInDiscovery(t *testing.T) {
	ctx := context.Background()
	registry := skills.NewRegistry(newTestStore(t))
	if err := registry.Register(ctx, skills.Skill{
		Name:           "disabled-skill",
		Version:        1,
		Summary:        "disabled",
		Content:        "disabled",
		CapabilityRefs: []string{"contract.current"},
		Enabled:        false,
	}); err != nil {
		t.Fatalf("register disabled skill: %v", err)
	}
	cfg := teamconfig.Config{
		TeamID:  "default-go-team",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			{ID: "builder", RuntimeProfile: "external-debug", CLIBackend: "codex", Skills: []string{"disabled-skill"}},
		},
	}
	got, err := registry.ListForAgent(ctx, cfg, "builder")
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("disabled skill was visible: %+v", got)
	}
}

func TestBuiltinSkillsKeepCommunicationAsWorkflowGuidanceNotSchema(t *testing.T) {
	var coordplane skills.Skill
	for _, skill := range skills.Builtins() {
		if skill.Name == "coordplane-service" {
			coordplane = skill
			break
		}
	}
	if coordplane.Name == "" {
		t.Fatal("coordplane-service builtin missing")
	}
	if !strings.Contains(coordplane.Content, "communication.read") || !containsName(coordplane.CapabilityRefs, "communication.read") {
		t.Fatalf("coordplane-service does not guide communication.read: %+v", coordplane)
	}
	for _, forbidden := range []string{"input_schema", "output_schema", "rejected_schema", `"properties"`} {
		if strings.Contains(coordplane.Content, forbidden) {
			t.Fatalf("skill content contains machine schema marker %q: %s", forbidden, coordplane.Content)
		}
	}
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
	})
	s := store.New(db)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func containsName(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
