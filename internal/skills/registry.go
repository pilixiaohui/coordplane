package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"coordplane/internal/store"
	"coordplane/internal/teamconfig"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Skill struct {
	Name           string   `json:"name"`
	Version        int      `json:"version"`
	Summary        string   `json:"summary"`
	Content        string   `json:"content"`
	CapabilityRefs []string `json:"capability_refs"`
	Enabled        bool     `json:"enabled"`
}

type Summary struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Summary string `json:"summary"`
}

type Registry struct {
	db *sql.DB
}

func NewRegistry(s *store.Store) *Registry {
	return &Registry{db: s.DB()}
}

func Builtins() []Skill {
	return []Skill{
		{
			Name:    "coordplane-service",
			Version: 1,
			Summary: "Read current work, mailbox, and submit repairable follow-up actions through CoordPlane capabilities.",
			Content: `# coordplane-service

Use this skill when you need to inspect your current assignment, read mailbox items, read authorized communication envelopes, send a message, or submit a completion report through CoordPlane.

Allowed workflow:
- Read current work with coordlink call contract.current.
- Read context with coordlink call contract.context when you need more detail.
- Read mailbox updates with coordlink call mailbox.list and mailbox.get, then call communication.read for the authorized envelope body when needed.
- Inspect and read durable report/artifact refs with object.inspect and object.read when a capability response gives you an object_ref.
- Run bounded workspace commands only through command.run when your TeamConfig grants it; treat the returned refs as evidence, not completion.
- Record verifier pass/fail/blocked findings only through validation.assessment when your TeamConfig grants it; normal reports are not canonical validation verdicts.
- Submit completion through contract.complete only after required evidence exists.

Do not use private backend internals or credentials. Treat rejected responses as repair instructions and continue in the same session.`,
			CapabilityRefs: []string{"contract.current", "contract.context", "mailbox.list", "mailbox.get", "mailbox.resolve", "communication.read", "message.send", "contract.complete", "object.inspect", "object.read", "command.run", "validation.assessment"},
			Enabled:        true,
		},
		{
			Name:    "contract-delegation",
			Version: 1,
			Summary: "Create accountable child contracts, wait for child results, and handle child-completed feedback.",
			Content: `# contract-delegation

Use this skill when you need another agent to complete a concrete deliverable.

Allowed workflow:
- Create a child contract with coordlink call contract.add.
- Use contract.wait when your current work is waiting on child output.
- When a child result arrives through mailbox, read the mailbox item, call communication.read for the result envelope when needed, and decide the next action yourself.

Do not treat a chat message as an accountable task. Do not rely on backend automation to make business decisions after a child completes.`,
			CapabilityRefs: []string{"contract.add", "contract.wait", "mailbox.list", "mailbox.get", "communication.read", "message.send"},
			Enabled:        true,
		},
		{
			Name:    "controlled-git",
			Version: 1,
			Summary: "Use audited workspace, Git, and changeset capabilities instead of direct unmanaged repository writes.",
			Content: `# controlled-git

Use this skill when you need to inspect or submit code changes through CoordPlane controlled Git.

Allowed workflow:
- Prepare or inspect your private workspace with workspace.prepare and workspace.status.
- Read repository state with git.status, git.diff, and git.log.
- Create commits only with git.commit and explicit paths.
- Submit or abandon tracked changes with changeset.submit and changeset.abandon.
- Preview and apply changes only through git.merge_preview and git.merge_apply.
- Resolve conflicts through git.conflicts, git.resolve, or git.abort; rollback applied merges with git.rollback.
- Use workspace.sync only when your workspace has no dirty tree or local commits.

Do not push, merge, rebase, or silently update canonical refs outside these capabilities.`,
			CapabilityRefs: []string{"workspace.prepare", "workspace.status", "workspace.sync", "git.status", "git.diff", "git.log", "git.commit", "changeset.submit", "changeset.abandon", "git.merge_preview", "git.merge_apply", "git.conflicts", "git.resolve", "git.abort", "git.rollback", "git.recover"},
			Enabled:        true,
		},
	}
}

func (r *Registry) Register(ctx context.Context, skill Skill) error {
	return r.register(ctx, skill, false)
}

func (r *Registry) register(ctx context.Context, skill Skill, upsert bool) error {
	if err := skill.Validate(); err != nil {
		return err
	}
	refsJSON, err := json.Marshal(skill.CapabilityRefs)
	if err != nil {
		return fmt.Errorf("marshal skill capability refs: %w", err)
	}
	enabled := 0
	if skill.Enabled {
		enabled = 1
	}
	stmt := `
INSERT INTO skill_packages (
  name, version, summary, content, capability_refs_json, enabled, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`
	if upsert {
		stmt += `
ON CONFLICT(name, version) DO UPDATE SET
  summary = excluded.summary,
  content = excluded.content,
  capability_refs_json = excluded.capability_refs_json,
  enabled = excluded.enabled`
	}
	_, err = r.db.ExecContext(ctx, stmt,
		skill.Name, skill.Version, skill.Summary, skill.Content, string(refsJSON), enabled, formatTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("register skill %s:%d: %w", skill.Name, skill.Version, err)
	}
	return nil
}

func (r *Registry) RegisterBuiltins(ctx context.Context) error {
	for _, skill := range Builtins() {
		if err := r.register(ctx, skill, true); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) ListForAgent(ctx context.Context, cfg teamconfig.Config, agentID string) ([]Summary, error) {
	agent, ok := cfg.Agent(agentID)
	if !ok {
		return nil, fmt.Errorf("skill list: agent %q is not declared in TeamConfig", agentID)
	}
	var out []Summary
	for _, name := range agent.Skills {
		skill, err := r.latestEnabled(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Summary{Name: skill.Name, Version: skill.Version, Summary: skill.Summary})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (r *Registry) ReadForAgent(ctx context.Context, cfg teamconfig.Config, agentID, name string) (Skill, error) {
	agent, ok := cfg.Agent(agentID)
	if !ok {
		return Skill{}, fmt.Errorf("skill read: agent %q is not declared in TeamConfig", agentID)
	}
	bound := false
	for _, skillName := range agent.Skills {
		if skillName == name {
			bound = true
			break
		}
	}
	if !bound {
		return Skill{}, fmt.Errorf("skill %q is not bound to agent %q", name, agentID)
	}
	return r.latestEnabled(ctx, name)
}

func (s Skill) Validate() error {
	if s.Name == "" {
		return errors.New("skill name is required")
	}
	if s.Version <= 0 {
		return fmt.Errorf("skill %q version must be positive", s.Name)
	}
	if s.Summary == "" {
		return fmt.Errorf("skill %q summary is required", s.Name)
	}
	if s.Content == "" {
		return fmt.Errorf("skill %q content is required", s.Name)
	}
	return nil
}

func (r *Registry) latestEnabled(ctx context.Context, name string) (Skill, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT name, version, summary, content, capability_refs_json, enabled
FROM skill_packages
WHERE name = ? AND enabled = 1
ORDER BY version DESC
LIMIT 1`, name)
	var skill Skill
	var refsJSON string
	var enabled int
	if err := row.Scan(&skill.Name, &skill.Version, &skill.Summary, &skill.Content, &refsJSON, &enabled); err != nil {
		return Skill{}, err
	}
	if err := json.Unmarshal([]byte(refsJSON), &skill.CapabilityRefs); err != nil {
		return Skill{}, fmt.Errorf("decode skill refs for %s: %w", name, err)
	}
	skill.Enabled = enabled == 1
	return cloneSkill(skill), nil
}

func cloneSkill(skill Skill) Skill {
	cloned := skill
	cloned.CapabilityRefs = append([]string(nil), skill.CapabilityRefs...)
	return cloned
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}
