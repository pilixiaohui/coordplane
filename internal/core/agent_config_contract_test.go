package core_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coordplane/internal/core"
	"coordplane/internal/store"
)

type agentConfigHarness struct {
	t        *testing.T
	database *store.Store
	service  *core.Service
	clock    *testClock
	ids      *testIDs
}

func newAgentConfigHarness(t *testing.T) *agentConfigHarness {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "coordplane.db"))
	requireNoError(t, err)
	clock := &testClock{value: time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)}
	ids := &testIDs{}
	git := &fakeGit{sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	service, err := core.NewService(database, git, core.ServiceOptions{
		Now: clock.Now, NewID: ids.New, MaxParallelRuns: 4,
		AdapterIDs: []string{"claude", "codex"},
		Adapters: []core.AdapterDescriptor{
			{
				ID: "claude", Name: "claude", ExecutionModel: "one_shot",
				SupportsResume: true, SupportsInject: false,
				AllowedEfforts: []string{"low", "medium", "high"},
			},
			{
				ID: "codex", Name: "codex", ExecutionModel: "one_shot",
				SupportsResume: true, SupportsInject: false,
				AllowedEfforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"},
			},
		},
	})
	requireNoError(t, err)
	service.SetReady(true, "")
	t.Cleanup(func() { _ = database.Close() })
	return &agentConfigHarness{t: t, database: database, service: service, clock: clock, ids: ids}
}

func validConfigInput() core.AgentConfigInput {
	return core.AgentConfigInput{
		DisplayName: "Config Agent", AdapterID: "claude", Image: "agent:latest",
		InstructionsText: "Follow the durable instructions.",
		Model:            "model-a", SubagentModel: "subagent-a",
		BaseURL: "https://example.invalid/v1", Effort: "medium",
	}
}

func addInputFromConfig(input core.AgentConfigInput) core.AddAgentInput {
	return core.AddAgentInput{
		DisplayName: input.DisplayName, AdapterID: input.AdapterID, Image: input.Image,
		InstructionsFile: input.InstructionsFile, InstructionsText: input.InstructionsText,
		Model: input.Model, SubagentModel: input.SubagentModel,
		BaseURL: input.BaseURL, Effort: input.Effort,
	}
}

func TestAgentConfigValidationRejectsUnsafeOrIncompleteInputWithoutWrites(t *testing.T) {
	h := newAgentConfigHarness(t)
	cases := []struct {
		name   string
		mutate func(*core.AgentConfigInput)
	}{
		{"empty display name", func(input *core.AgentConfigInput) { input.DisplayName = " " }},
		{"unregistered adapter", func(input *core.AgentConfigInput) { input.AdapterID = "retired" }},
		{"empty image", func(input *core.AgentConfigInput) { input.Image = "" }},
		{"both instruction sources", func(input *core.AgentConfigInput) { input.InstructionsFile = "/instructions/agent.md" }},
		{"no instruction source", func(input *core.AgentConfigInput) { input.InstructionsText = "" }},
		{"relative instruction file", func(input *core.AgentConfigInput) {
			input.InstructionsText, input.InstructionsFile = "", "instructions/agent.md"
		}},
		{"non-canonical instruction file", func(input *core.AgentConfigInput) {
			input.InstructionsText, input.InstructionsFile = "", "/instructions/../agent.md"
		}},
		{"oversized instruction text", func(input *core.AgentConfigInput) {
			input.InstructionsText = strings.Repeat("x", core.MaximumInstructionsBytes+1)
		}},
		{"invalid UTF-8 instruction text", func(input *core.AgentConfigInput) {
			input.InstructionsText = string([]byte{0xff, 0xfe})
		}},
		{"unsafe model token", func(input *core.AgentConfigInput) { input.Model = "model; rm -rf" }},
		{"newline subagent model", func(input *core.AgentConfigInput) { input.SubagentModel = "sub\nagent" }},
		{"non-https base URL", func(input *core.AgentConfigInput) { input.BaseURL = "http://example.invalid/v1" }},
		{"base URL userinfo", func(input *core.AgentConfigInput) { input.BaseURL = "https://user:pass@example.invalid/v1" }},
		{"base URL query", func(input *core.AgentConfigInput) { input.BaseURL = "https://example.invalid/v1?token=x" }},
		{"base URL fragment", func(input *core.AgentConfigInput) { input.BaseURL = "https://example.invalid/v1#frag" }},
		{"unknown effort", func(input *core.AgentConfigInput) { input.Effort = "ultra" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := validConfigInput()
			test.mutate(&input)
			before := durableSignature(t, h.database, "")
			_, err := h.service.AddAgent(context.Background(), addInputFromConfig(input))
			if !core.IsCode(err, core.CodeInvalidArgument) {
				t.Fatalf("error = %v, want %s", err, core.CodeInvalidArgument)
			}
			h.requireAgentDurableState(t, before)
		})
	}
}

func TestListAdaptersReturnsSortedDetachedReadOnlyMetadata(t *testing.T) {
	h := newAgentConfigHarness(t)
	descriptors, err := h.service.ListAdapters(context.Background())
	requireNoError(t, err)
	if len(descriptors) != 2 || descriptors[0].ID != "claude" || descriptors[1].ID != "codex" {
		t.Fatalf("adapter descriptors = %#v", descriptors)
	}
	if descriptors[0].Name != "claude" || descriptors[0].ExecutionModel != "one_shot" ||
		!descriptors[0].SupportsResume || descriptors[0].SupportsInject ||
		len(descriptors[0].AllowedEfforts) != 3 || descriptors[0].AllowedEfforts[0] != "low" {
		t.Fatalf("Claude descriptor = %#v", descriptors[0])
	}
	if descriptors[1].Name != "codex" || descriptors[1].ExecutionModel != "one_shot" ||
		!descriptors[1].SupportsResume || descriptors[1].SupportsInject ||
		len(descriptors[1].AllowedEfforts) != 6 || descriptors[1].AllowedEfforts[5] != "ultra" {
		t.Fatalf("Codex descriptor = %#v", descriptors[1])
	}
	descriptors[0].AllowedEfforts[0] = "tampered"
	again, err := h.service.ListAdapters(context.Background())
	requireNoError(t, err)
	if again[0].AllowedEfforts[0] != "low" {
		t.Fatalf("ListAdapters exposed its internal effort slice: %#v", again[0].AllowedEfforts)
	}
	raw, err := json.Marshal(descriptors)
	requireNoError(t, err)
	for _, forbidden := range []string{"executable", "argv", "secret", "password", "/usr/bin"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("adapter descriptors leaked read-only boundary %q: %s", forbidden, raw)
		}
	}
}

func TestAddAgentPersistsAllConfigFieldsAndMirrorsParticipant(t *testing.T) {
	h := newAgentConfigHarness(t)
	input := validConfigInput()
	agent, err := h.service.AddAgent(context.Background(), addInputFromConfig(input))
	requireNoError(t, err)
	if agent.InstructionsText != input.InstructionsText || agent.Model != input.Model ||
		agent.SubagentModel != input.SubagentModel || agent.BaseURL != input.BaseURL ||
		agent.Effort != input.Effort || agent.Version != 1 {
		t.Fatalf("added Agent = %#v", agent)
	}
	h.requireMirror(t, agent.ID)
}

func TestUpdateAgentCASMirrorsBothRowsAndRedactsChangedValues(t *testing.T) {
	h := newAgentConfigHarness(t)
	added, err := h.service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: "Before Update", AdapterID: "claude", Image: "agent:before",
		InstructionsFile: "/instructions/before.md", RequestID: "add-agent-before-update",
	})
	requireNoError(t, err)
	replacement := validConfigInput()
	replacement.DisplayName = "After Update"
	secretPrompt := "TOP-SECRET prompt body must never enter an Event"
	replacement.InstructionsText = secretPrompt
	replacement.BaseURL = "https://private.invalid/v1"

	updated, err := h.service.UpdateAgent(context.Background(), core.UpdateAgentInput{
		ID: added.ID, Version: added.Version, AgentConfigInput: replacement, RequestID: "update-agent",
	})
	requireNoError(t, err)
	if updated.Version != added.Version+1 || updated.DisplayName != replacement.DisplayName ||
		updated.InstructionsFile != "" || updated.InstructionsText != secretPrompt {
		t.Fatalf("updated Agent = %#v", updated)
	}
	h.requireMirror(t, added.ID)

	events, err := h.database.Events(context.Background(), core.EventFilter{EntityType: "agent", EntityID: added.ID})
	requireNoError(t, err)
	if len(events) != 2 || events[1].Kind != "agent.updated" {
		t.Fatalf("agent events = %#v", events)
	}
	payload := events[1].PayloadJSON
	for _, forbidden := range []string{secretPrompt, replacement.BaseURL, replacement.Model, replacement.Effort, "/instructions/before.md"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("agent.updated Event leaked %q: %s", forbidden, payload)
		}
	}
	var decoded struct {
		Version       int64    `json:"version"`
		ChangedFields []string `json:"changed_fields"`
	}
	requireNoError(t, json.Unmarshal([]byte(payload), &decoded))
	if decoded.Version != updated.Version {
		t.Fatalf("agent.updated version = %d, want %d", decoded.Version, updated.Version)
	}
	for _, field := range []string{"display_name", "image", "instructions_file", "instructions_text", "model", "subagent_model", "base_url", "effort"} {
		if !containsString(decoded.ChangedFields, field) {
			t.Fatalf("agent.updated changed_fields %v omitted %q", decoded.ChangedFields, field)
		}
	}

	before := h.agentDurableState(added.ID)
	_, err = h.service.UpdateAgent(context.Background(), core.UpdateAgentInput{
		ID: added.ID, Version: added.Version, AgentConfigInput: replacement, RequestID: "stale-update",
	})
	if !core.IsCode(err, core.CodeVersionConflict) {
		t.Fatalf("stale update error = %v, want %s", err, core.CodeVersionConflict)
	}
	if after := h.agentDurableState(added.ID); after != before {
		t.Fatalf("stale update changed durable state\nbefore=%s\nafter=%s", before, after)
	}
}

func TestUpdateAgentRequestIDReplayIsIdempotent(t *testing.T) {
	h := newAgentConfigHarness(t)
	added, err := h.service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: "Idempotent Update", AdapterID: "claude", Image: "agent:latest",
		InstructionsText: "before", RequestID: "add-idempotent-update",
	})
	requireNoError(t, err)
	replacement := validConfigInput()
	replacement.DisplayName = "Idempotent After"
	input := core.UpdateAgentInput{
		ID: added.ID, Version: added.Version, AgentConfigInput: replacement,
		RequestID: "update-idempotent",
	}
	first, err := h.service.UpdateAgent(context.Background(), input)
	requireNoError(t, err)
	events, err := h.database.Events(context.Background(), core.EventFilter{EntityType: "agent", EntityID: added.ID})
	requireNoError(t, err)
	replayed, err := h.service.UpdateAgent(context.Background(), input)
	requireNoError(t, err)
	replayedEvents, err := h.database.Events(context.Background(), core.EventFilter{EntityType: "agent", EntityID: added.ID})
	requireNoError(t, err)
	if replayed.Version != first.Version || replayed.UpdatedAt != first.UpdatedAt ||
		replayed.DisplayName != "Idempotent After" || len(replayedEvents) != len(events) {
		t.Fatalf("replayed update = %#v (events %d -> %d), want unchanged durable result",
			replayed, len(events), len(replayedEvents))
	}
}

func TestAgentStatusAndArchiveMirrorParticipantInSameTransaction(t *testing.T) {
	h := newAgentConfigHarness(t)
	agent, err := h.service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: "Status Mirror", AdapterID: "claude", Image: "agent:latest",
		InstructionsText: "mirror status", RequestID: "add-status-mirror",
	})
	requireNoError(t, err)
	paused, err := h.service.SetAgentStatus(context.Background(), agent.ID, core.AgentPaused, "pause-mirror")
	requireNoError(t, err)
	participant, err := h.database.Participant(context.Background(), agent.ID)
	requireNoError(t, err)
	if paused.Status != core.AgentPaused || paused.Version != 2 ||
		participant.Status != string(core.AgentPaused) || participant.Version != 2 {
		t.Fatalf("pause mirror Agent=%#v Participant=%#v", paused, participant)
	}
	archived, err := h.service.ArchiveAgent(context.Background(), agent.ID, "archive-mirror")
	requireNoError(t, err)
	participant, err = h.database.Participant(context.Background(), agent.ID)
	requireNoError(t, err)
	if archived.Status != core.AgentArchived || archived.Version != 3 ||
		participant.Status != string(core.AgentArchived) || participant.Version != 3 {
		t.Fatalf("archive mirror Agent=%#v Participant=%#v", archived, participant)
	}
}

func TestAgentManageGateProtectsAddAndUpdate(t *testing.T) {
	h := newAgentConfigHarness(t)
	existing, err := h.service.AddAgent(context.Background(), core.AddAgentInput{
		DisplayName: "Gate Existing", AdapterID: "claude", Image: "agent:latest",
		InstructionsText: "existing", RequestID: "add-gate-existing",
	})
	requireNoError(t, err)
	capabilities := core.CapabilityNames(core.AllCapabilities())
	withoutAgentManage := make([]string, 0, len(capabilities)-1)
	for _, capability := range capabilities {
		if capability != string(core.CapabilityAgentManage) {
			withoutAgentManage = append(withoutAgentManage, capability)
		}
	}
	role, err := h.service.CreateRole(context.Background(), core.RoleInput{
		Name: "operator-without-agent-manage", Description: "no agent.manage",
		Capabilities: withoutAgentManage, RequestID: "create-no-agent-manage-role",
	})
	requireNoError(t, err)
	_, err = h.service.BindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID,
		RoleID: role.ID, RequestID: "bind-no-agent-manage-role",
	})
	requireNoError(t, err)
	requireNoError(t, h.service.UnbindParticipantRole(context.Background(), core.BindRoleInput{
		ParticipantID: core.DefaultHumanParticipantID, ProjectID: core.GlobalProjectID,
		RoleID: core.DefaultOwnerRoleID, RequestID: "unbind-owner-role",
	}))

	before := durableSignature(t, h.database, "")
	if _, err := h.service.AddAgent(context.Background(), addInputFromConfig(validConfigInput())); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("AddAgent without agent.manage error = %v, want %s", err, core.CodeScopeDenied)
	}
	if _, err := h.service.UpdateAgent(context.Background(), core.UpdateAgentInput{
		ID: existing.ID, Version: existing.Version,
		AgentConfigInput: validConfigInput(), RequestID: "denied-update",
	}); !core.IsCode(err, core.CodeScopeDenied) {
		t.Fatalf("UpdateAgent without agent.manage error = %v, want %s", err, core.CodeScopeDenied)
	}
	h.requireAgentDurableState(t, before)
}

func (h *agentConfigHarness) requireMirror(t *testing.T, agentID string) {
	t.Helper()
	agent, err := h.database.Agent(context.Background(), agentID)
	requireNoError(t, err)
	participant, err := h.database.Participant(context.Background(), agentID)
	requireNoError(t, err)
	if participant.Kind != core.ParticipantKindCLIAgent ||
		participant.DisplayName != agent.DisplayName ||
		participant.Status != string(agent.Status) ||
		participant.AdapterID != agent.AdapterID ||
		participant.Image != agent.Image ||
		participant.InstructionsFile != agent.InstructionsFile ||
		participant.InstructionsText != agent.InstructionsText ||
		participant.Model != agent.Model ||
		participant.SubagentModel != agent.SubagentModel ||
		participant.BaseURL != agent.BaseURL ||
		participant.Effort != agent.Effort ||
		participant.Version != agent.Version {
		t.Fatalf("participant does not mirror Agent\nAgent=%#v\nParticipant=%#v", agent, participant)
	}
}

func (h *agentConfigHarness) agentDurableState(agentID string) string {
	h.t.Helper()
	agent, err := h.database.Agent(context.Background(), agentID)
	if err != nil && core.IsCode(err, core.CodeNotFound) {
		return "agent-not-found"
	}
	requireNoError(h.t, err)
	participant, err := h.database.Participant(context.Background(), agentID)
	if err != nil && core.IsCode(err, core.CodeNotFound) {
		return "agent-not-found"
	}
	requireNoError(h.t, err)
	events, err := h.database.Events(context.Background(), core.EventFilter{EntityType: "agent", EntityID: agentID})
	requireNoError(h.t, err)
	raw, err := json.Marshal([3]any{agent, participant, events})
	requireNoError(h.t, err)
	return string(raw)
}

func (h *agentConfigHarness) requireAgentDurableState(t *testing.T, want string) {
	t.Helper()
	// The empty project scope covers the global Agent/Participant/Event rows
	// touched by validation failures; the per-entity helper below is used when
	// an exact Agent ID exists.
	if got := durableSignature(t, h.database, ""); got != want {
		t.Fatalf("durable state changed\nbefore=%s\nafter=%s", want, got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
