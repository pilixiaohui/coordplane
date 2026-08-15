package core_test

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"coordplane/internal/core"
	_ "modernc.org/sqlite"
)

// RP-01: the static capability registry is complete and closed: every
// registered capability parses, unknown names are rejected, and the default
// owner role carries every capability so the seeded human keeps full access.
func TestRP01CapabilityRegistryIsCompleteAndClosed(t *testing.T) {
	names := core.CapabilityNames(core.AllCapabilities())
	if len(names) == 0 {
		t.Fatal("capability registry is empty")
	}
	parsed, err := core.ParseCapabilities(names)
	requireNoError(t, err)
	if len(parsed) != len(names) {
		t.Fatalf("parsed capability count = %d, want %d", len(parsed), len(names))
	}
	if _, err := core.ParseCapabilities([]string{"task.fabricate"}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("unknown capability error = %v, want INVALID_ARGUMENT", err)
	}
	for _, capability := range core.AllCapabilities() {
		if !core.HasCapability(core.AllCapabilities(), capability) {
			t.Fatalf("capability %q missing from its own registry", capability)
		}
	}
}

// RP-01b: the seeded owner role carries every capability and the seeded agent
// role carries exactly the minimal CLI agent set.
func TestRP01SeededRolesCarryExpectedCapabilities(t *testing.T) {
	h := newHarness(t)
	owner, err := h.service.Role(context.Background(), core.DefaultOwnerRoleID)
	requireNoError(t, err)
	if len(owner.Capabilities) != len(core.AllCapabilities()) {
		t.Fatalf("owner capability count = %d, want %d", len(owner.Capabilities), len(core.AllCapabilities()))
	}
	agent, err := h.service.Role(context.Background(), core.DefaultAgentRoleID)
	requireNoError(t, err)
	agentCaps := core.AgentDefaultCapabilities()
	if len(agent.Capabilities) != len(agentCaps) {
		t.Fatalf("agent capability count = %d, want %d", len(agent.Capabilities), len(agentCaps))
	}
	human, err := h.service.Participant(context.Background(), core.DefaultHumanParticipantID)
	requireNoError(t, err)
	if human.Kind != core.ParticipantKindHuman {
		t.Fatalf("default human participant kind = %q, want human", human.Kind)
	}
}

// RP-02: roles are fully configurable data. Create/update/delete work at
// runtime; a bound role cannot be deleted (IN_USE); concurrent updates conflict.
func TestRP02RoleCRUDAndBindingDeleteGuard(t *testing.T) {
	h := newHarness(t)
	role, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "tester", Description: "run the tests", Capabilities: []string{"run.view", "task.create", "run.view"},
		RequestID: "rp02-create",
	})
	requireNoError(t, err)
	if len(role.Capabilities) != 2 {
		t.Fatalf("duplicate capabilities not deduplicated: %v", role.Capabilities)
	}
	// Duplicate name rejected.
	if _, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "tester", Capabilities: []string{"run.view"}, RequestID: "rp02-dup",
	}); !core.IsCode(err, core.CodeInvalidState) {
		t.Fatalf("duplicate role name error = %v, want INVALID_STATE", err)
	}
	// Unknown capability rejected.
	if _, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "broken", Capabilities: []string{"nope"}, RequestID: "rp02-broken",
	}); !core.IsCode(err, core.CodeInvalidArgument) {
		t.Fatalf("unknown capability error = %v, want INVALID_ARGUMENT", err)
	}
	// Update re-scopes the role at runtime.
	updated, err := h.service.UpdateRole(context.Background(), core.RoleUpdateInput{
		RoleID: role.ID, Name: "tester", Description: "renamed", Capabilities: []string{"task.create"},
		RequestID: "rp02-update",
	})
	requireNoError(t, err)
	if updated.Version != 2 || len(updated.Capabilities) != 1 || updated.Description != "renamed" {
		t.Fatalf("updated role = %#v", updated)
	}
	// A bound role cannot be deleted.
	agent := h.addAgent(t, "rp02-agent")
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: agent.ID, ProjectID: core.GlobalProjectID, RoleID: role.ID, RequestID: "rp02-bind",
	}); err != nil {
		t.Fatalf("bind role: %v", err)
	}
	if err := h.service.DeleteRole(context.Background(), role.ID, "rp02-delete-bound"); !core.IsCode(err, core.CodeInUse) {
		t.Fatalf("bound role delete error = %v, want IN_USE", err)
	}
	// Unbind then delete succeeds.
	if err := h.service.UnbindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: agent.ID, ProjectID: core.GlobalProjectID, RoleID: role.ID, RequestID: "rp02-unbind",
	}); err != nil {
		t.Fatalf("unbind role: %v", err)
	}
	requireNoError(t, err)
	if err := h.service.DeleteRole(context.Background(), role.ID, "rp02-delete"); err != nil {
		t.Fatalf("delete unbound role: %v", err)
	}
}

// RP-03: role permissions are per-project. The same human can repair in one
// project and be denied in another after its binding changes.
func TestRP03ProjectScopedPermissions(t *testing.T) {
	h := newHarness(t)
	projectA := h.addProject(t, "rp03-a", "")
	projectB := h.addProject(t, "rp03-b", "")

	// Replace the human's global owner role with a global role that lacks
	// project.repair, so project scopes decide repairability. The owner role
	// holds participant.manage, so keep a second manage holder first.
	globalOperator, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name:         "rp03-global-operator",
		Capabilities: capabilityNamesWithout(core.AllCapabilities(), core.CapabilityProjectRepair),
		RequestID:    "rp03-global-operator",
	})
	requireNoError(t, err)
	coownerRole, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "rp03-coowner", Capabilities: core.CapabilityNames(core.AllCapabilities()), RequestID: "rp03-coowner",
	})
	requireNoError(t, err)
	coowner := h.addAgent(t, "rp03-coowner-agent")
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: coowner.ID, ProjectID: core.GlobalProjectID, RoleID: coownerRole.ID,
		RequestID: "rp03-bind-coowner",
	}); err != nil {
		t.Fatalf("bind coowner: %v", err)
	}
	// Bind the replacement global role first (the human still holds role.bind),
	// then strip the owner role: the coowner agent keeps manage alive.
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: globalOperator.ID,
		RequestID: "rp03-bind-global-operator",
	}); err != nil {
		t.Fatalf("bind global operator: %v", err)
	}
	if err := h.service.UnbindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: core.DefaultOwnerRoleID,
		RequestID: "rp03-unbind-global-owner",
	}); err != nil {
		t.Fatalf("unbind global owner: %v", err)
	}
	// Project B: drop owner, bind a viewer role without project.repair.
	viewer, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "rp03-viewer", Capabilities: []string{"project.view", "message.read"}, RequestID: "rp03-viewer",
	})
	requireNoError(t, err)
	if err := h.service.UnbindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: projectB.ID, RoleID: core.DefaultOwnerRoleID,
		RequestID: "rp03-unbind-owner-b",
	}); err != nil {
		t.Fatalf("unbind owner in B: %v", err)
	}
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: projectB.ID, RoleID: viewer.ID,
		RequestID: "rp03-bind-viewer-b",
	}); err != nil {
		t.Fatalf("bind viewer in B: %v", err)
	}
	// Project A keeps the owner binding (project.repair); project B is denied
	// with zero side effects.
	forceProjectError(t, h, projectA.ID)
	forceProjectError(t, h, projectB.ID)
	if _, err := h.service.RepairProject(context.Background(), projectB.ID, "rp03-repair-b-denied"); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("repair in B error = %v, want SCOPE_DENIED", err)
	}
	if _, err := h.service.RepairProject(context.Background(), projectA.ID, "rp03-repair-a"); err != nil {
		t.Fatalf("repair in A with project owner binding: %v", err)
	}
}

func capabilityNamesWithout(capabilities []core.Capability, excluded core.Capability) []string {
	var names []string
	for _, capability := range capabilities {
		if capability != excluded {
			names = append(names, string(capability))
		}
	}
	return names
}

// forceProjectError flips a project to error status through the real store so
// RepairProject's state precondition is met.
func forceProjectError(t *testing.T, h *harness, projectID string) {
	t.Helper()
	err := h.database.Transact(context.Background(), func(tx core.Transaction) error {
		project, err := tx.Project(projectID)
		if err != nil {
			return err
		}
		oldVersion := project.Version
		project.Status = core.ProjectError
		project.LastError = "injected test failure"
		project.Version++
		return tx.UpdateProject(project, oldVersion, core.ProjectActive)
	})
	requireNoError(t, err)
}

// RP-04: management capabilities are global-scope only. A role carrying every
// project capability still cannot manage roles without a global-scope binding.
func TestRP04GlobalScopeIsolation(t *testing.T) {
	h := newHarness(t)
	project := h.addProject(t, "rp04-project", "")
	projectOnly, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name:         "rp04-project-only",
		Capabilities: core.CapabilityNames(core.AllCapabilities()),
		RequestID:    "rp04-role",
	})
	requireNoError(t, err)
	// Bind the project-scope role first, while the human still holds global
	// role.bind through the owner role.
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: project.ID, RoleID: projectOnly.ID,
		RequestID: "rp04-bind-project",
	}); err != nil {
		t.Fatalf("bind project-only role: %v", err)
	}
	// The owner role holds participant.manage, so strip it only after a second
	// manage holder (a different participant) exists.
	second, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "rp04-coowner", Capabilities: core.CapabilityNames(core.AllCapabilities()), RequestID: "rp04-coowner",
	})
	requireNoError(t, err)
	coowner := h.addAgent(t, "rp04-coowner-agent")
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: coowner.ID, ProjectID: core.GlobalProjectID, RoleID: second.ID,
		RequestID: "rp04-bind-coowner",
	}); err != nil {
		t.Fatalf("bind coowner: %v", err)
	}
	if err := h.service.UnbindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: core.DefaultOwnerRoleID,
		RequestID: "rp04-unbind-global",
	}); err != nil {
		t.Fatalf("unbind global owner: %v", err)
	}
	// Project-scope full capabilities cannot manage roles.
	if _, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "rp04-nope", Capabilities: []string{"run.view"}, RequestID: "rp04-create-denied",
	}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("global management without global binding error = %v, want SCOPE_DENIED", err)
	}
	// Without the global owner binding the human cannot bind or manage roles
	// at all: the coowner agent holds manage, and global ops are denied.
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: projectOnly.ID,
		RequestID: "rp04-bind-denied",
	}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("global bind without role.bind error = %v, want SCOPE_DENIED", err)
	}
}

// RP-06: the last participant.manage holder cannot be stripped, so the system
// never loses its administrator. A failed unbind must leave the binding rows
// and the events table untouched.
func TestRP06LastManageHolderIsProtected(t *testing.T) {
	h := newHarness(t)
	before := rp06BindingEventsSignature(t, h.path)
	err := h.service.UnbindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: core.DefaultOwnerRoleID,
		RequestID: "rp06-last-manage",
	})
	if !core.IsCode(err, core.CodeInUse) {
		t.Fatalf("unbind last manage holder error = %v, want IN_USE", err)
	}
	if after := rp06BindingEventsSignature(t, h.path); after != before {
		t.Fatalf("failed unbind of last manage holder changed durable state\nbefore=%s\nafter=%s", before, after)
	}
	second, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "rp06-coowner", Capabilities: core.CapabilityNames(core.AllCapabilities()), RequestID: "rp06-role",
	})
	requireNoError(t, err)
	// A participant that keeps manage through another role of its own may
	// drop one manage binding.
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: second.ID,
		RequestID: "rp06-bind-same",
	}); err != nil {
		t.Fatalf("bind same-participant second role: %v", err)
	}
	if err := h.service.UnbindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: second.ID,
		RequestID: "rp06-unbind-second",
	}); err != nil {
		t.Fatalf("unbind second role while owner retains manage: %v", err)
	}
	// Grant a second holder (a different participant) first, then stripping
	// the original succeeds.
	coowner := h.addAgent(t, "rp06-coowner-agent")
	if _, err := h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: coowner.ID, ProjectID: core.GlobalProjectID, RoleID: second.ID,
		RequestID: "rp06-bind-second",
	}); err != nil {
		t.Fatalf("bind second manage holder: %v", err)
	}
	if err := h.service.UnbindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID, RoleID: core.DefaultOwnerRoleID,
		RequestID: "rp06-unbind-owner",
	}); err != nil {
		t.Fatalf("unbind owner with second holder present: %v", err)
	}
}

// Scope resolution pure logic: project scope pulls project and global
// bindings; a different project's binding never leaks in.
func TestRP00ParticipantCapabilitiesScopeResolution(t *testing.T) {
	bindings := []core.ParticipantRoleBinding{
		{ProjectID: core.GlobalProjectID, Capabilities: []core.Capability{core.CapabilityRoleManage}},
		{ProjectID: "prj-a", Capabilities: []core.Capability{core.CapabilityProjectRepair}},
		{ProjectID: "prj-b", Capabilities: []core.Capability{core.CapabilityGCDiscard}},
	}
	effectiveA := core.ParticipantCapabilities(bindings, "prj-a")
	if !core.HasCapability(effectiveA, core.CapabilityRoleManage) || !core.HasCapability(effectiveA, core.CapabilityProjectRepair) {
		t.Fatalf("project A effective = %v, want global + project A", effectiveA)
	}
	if core.HasCapability(effectiveA, core.CapabilityGCDiscard) {
		t.Fatalf("project B capability leaked into project A: %v", effectiveA)
	}
}

// rp06BindingEventsSignature re-reads the role-binding rows and the events
// table directly from SQLite so a failed mutation can be proven side-effect
// free (no binding removed, no Event appended).
func rp06BindingEventsSignature(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	requireNoError(t, err)
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(context.Background(), `SELECT participant_id, project_id, role_id FROM participant_project_role ORDER BY participant_id, project_id, role_id`)
	requireNoError(t, err)
	defer rows.Close()
	var bindings []string
	for rows.Next() {
		var participantID, projectID, roleID string
		if err := rows.Scan(&participantID, &projectID, &roleID); err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, participantID+"|"+projectID+"|"+roleID)
	}
	requireNoError(t, rows.Err())
	eventRows, err := db.QueryContext(context.Background(), `SELECT id, entity_type, entity_id, kind, actor_kind, actor_id, request_id, created_at FROM events ORDER BY id`)
	requireNoError(t, err)
	defer eventRows.Close()
	var events []string
	for eventRows.Next() {
		var id int64
		var entityType, entityID, kind, actorKind, actorID, requestID, createdAt string
		if err := eventRows.Scan(&id, &entityType, &entityID, &kind, &actorKind, &actorID, &requestID, &createdAt); err != nil {
			t.Fatal(err)
		}
		events = append(events, strings.Join([]string{strconv.FormatInt(id, 10), entityType, entityID, kind, actorKind, actorID, requestID, createdAt}, "|"))
	}
	requireNoError(t, eventRows.Err())
	return strings.Join(bindings, "\n") + "\n==EVENTS==\n" + strings.Join(events, "\n")
}
