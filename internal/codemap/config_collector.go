package codemap

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type ConfigCollector struct{}

func (ConfigCollector) Name() string    { return "config" }
func (ConfigCollector) Version() string { return "v1" }

type teamConfigSummary struct {
	TeamID          string                    `yaml:"team_id"`
	Version         int                       `yaml:"version"`
	Agents          []teamConfigAgentSummary  `yaml:"agents"`
	RuntimeProfiles map[string]map[string]any `yaml:"runtime_profiles"`
	Communication   map[string]any            `yaml:"communication"`
	Termination     map[string]any            `yaml:"termination"`
	Extra           map[string]any            `yaml:",inline"`
}

type teamConfigAgentSummary struct {
	ID             string   `yaml:"id"`
	RuntimeProfile string   `yaml:"runtime_profile"`
	CLIBackend     string   `yaml:"cli_backend"`
	Skills         []string `yaml:"skills"`
	Capabilities   []string `yaml:"capabilities"`
}

func (collector ConfigCollector) Collect(ctx context.Context, collectCtx CollectContext) (Collection, error) {
	files, err := walkRelativeFiles(collectCtx.Root, func(rel string) bool {
		if !strings.HasPrefix(rel, "team_config/fixtures/") {
			return false
		}
		lower := strings.ToLower(rel)
		return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
	})
	if err != nil {
		return Collection{}, err
	}
	var collection Collection
	for _, rel := range files {
		select {
		case <-ctx.Done():
			return Collection{}, ctx.Err()
		default:
		}
		fullPath := filepath.Join(collectCtx.Root, filepath.FromSlash(rel))
		raw, digest, err := readFileDigest(fullPath)
		if err != nil {
			collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
				Severity: DiagnosticError,
				Code:     "CODMAP_CONFIG_READ_FAILED",
				Path:     rel,
				Message:  err.Error(),
			})
			continue
		}
		collection.InputFiles = append(collection.InputFiles, InputFile{Path: rel, Digest: digest})
		var cfg teamConfigSummary
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			collection.Diagnostics = append(collection.Diagnostics, Diagnostic{
				Severity:   DiagnosticWarning,
				Code:       "CODMAP_TEAM_CONFIG_PARSE_FAILED",
				Path:       rel,
				Message:    err.Error(),
				RepairHint: "fix fixture YAML before relying on TeamConfig codemap metadata",
			})
		}
		agentIDs, skills, capabilities := summarizeAgents(cfg.Agents)
		fixtureID := StableNodeID(NodeKindFixture, rel, rel)
		collection.Nodes = append(collection.Nodes, Node{
			ID:         fixtureID,
			Kind:       NodeKindFixture,
			Name:       filepath.Base(rel),
			Path:       rel,
			Digest:     digest,
			Visibility: "repo",
			Source:     "evidence",
			Confidence: 1,
			Metadata: map[string]any{
				"type":         "team_config",
				"team_id":      cfg.TeamID,
				"version":      cfg.Version,
				"agent_ids":    agentIDs,
				"skills":       skills,
				"capabilities": capabilities,
			},
		})
		if strings.TrimSpace(cfg.TeamID) == "" {
			continue
		}
		name := cfg.TeamID
		if cfg.Version > 0 {
			name = name + "@" + intString(cfg.Version)
		}
		configID := StableNodeID(NodeKindTeamConfig, rel, name)
		collection.Nodes = append(collection.Nodes, Node{
			ID:         configID,
			Kind:       NodeKindTeamConfig,
			Name:       name,
			Path:       rel,
			Visibility: "repo",
			Source:     "evidence",
			Confidence: 1,
			Metadata: map[string]any{
				"team_id":          cfg.TeamID,
				"version":          cfg.Version,
				"agent_count":      len(cfg.Agents),
				"runtime_profiles": sortedMapKeys(cfg.RuntimeProfiles),
			},
		})
		evidence := []Evidence{{Path: rel, Collector: "config"}}
		collection.Edges = append(collection.Edges, Edge{
			ID:         StableEdgeID(EdgeKindContains, fixtureID, configID, evidence),
			FromID:     fixtureID,
			ToID:       configID,
			Kind:       EdgeKindContains,
			Evidence:   evidence,
			Confidence: 1,
		})
	}
	return collection, nil
}

func summarizeAgents(agents []teamConfigAgentSummary) ([]string, []string, []string) {
	agentSet := make(map[string]bool)
	skillSet := make(map[string]bool)
	capabilitySet := make(map[string]bool)
	for _, agent := range agents {
		if agent.ID != "" {
			agentSet[agent.ID] = true
		}
		for _, skill := range agent.Skills {
			if strings.TrimSpace(skill) != "" {
				skillSet[skill] = true
			}
		}
		for _, capability := range agent.Capabilities {
			if strings.TrimSpace(capability) != "" {
				capabilitySet[capability] = true
			}
		}
	}
	return sortedSet(agentSet), sortedSet(skillSet), sortedSet(capabilitySet)
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedMapKeys[V any](values map[string]V) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
