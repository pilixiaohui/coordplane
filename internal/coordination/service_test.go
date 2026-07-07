package coordination_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"coordplane/internal/adapters/coordlink"
	"coordplane/internal/adapters/httpapi"
	"coordplane/internal/capability"
	"coordplane/internal/coordination"
	"coordplane/internal/objects"
	"coordplane/internal/policy"
	"coordplane/internal/store"
	"coordplane/internal/teamconfig"

	_ "modernc.org/sqlite"
)

func TestMessageSendDoesNotChangeContractStatus(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	work := addAndClaim(t, ctx, svc, "builder")

	resp := svc.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          work.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "note",
		Body:             "status update only",
	})
	if err := resp.Validate(); err != nil {
		t.Fatalf("validate message response: %v", err)
	}
	if resp.Status != capability.StatusAccepted {
		t.Fatalf("message response = %+v, want accepted", resp)
	}

	status := contractStatus(t, ctx, db, work.Contract.ID)
	if status != "open" {
		t.Fatalf("contract status after message.send = %s, want open", status)
	}
	if countRows(t, ctx, db, "messages") != 1 {
		t.Fatalf("messages row count = %d, want 1", countRows(t, ctx, db, "messages"))
	}
}

func TestMessageSendTaskLikeBodyCreatesOnlyMessageEnvelope(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	work := addAndClaim(t, ctx, svc, "builder")
	contractsBefore := countRows(t, ctx, db, "work_contracts")
	assignmentsBefore := countRows(t, ctx, db, "assignments")

	resp := svc.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          work.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "note",
		Body:             "please deliver a feature; this still must not become a contract",
	})
	if resp.Status != capability.StatusAccepted || resp.Data == nil {
		t.Fatalf("task-like message response = %+v, want accepted", resp)
	}
	if resp.Data.EnvelopeID == "" || envelopeKind(t, ctx, db, resp.Data.EnvelopeID) != "message" {
		t.Fatalf("message envelope = %+v, kind=%q", resp.Data, envelopeKind(t, ctx, db, resp.Data.EnvelopeID))
	}
	if countRows(t, ctx, db, "messages") != 1 {
		t.Fatalf("messages row count = %d, want 1", countRows(t, ctx, db, "messages"))
	}
	if got := countRows(t, ctx, db, "work_contracts"); got != contractsBefore {
		t.Fatalf("task-like message created work_contracts: got %d want %d", got, contractsBefore)
	}
	if got := countRows(t, ctx, db, "assignments"); got != assignmentsBefore {
		t.Fatalf("task-like message created assignments: got %d want %d", got, assignmentsBefore)
	}
	if mailboxEnvelopeID(t, ctx, db, resp.Data.MailboxID) != resp.Data.EnvelopeID {
		t.Fatalf("mailbox does not point to message envelope")
	}
	if contractStatus(t, ctx, db, work.Contract.ID) != "open" {
		t.Fatalf("task request changed contract status")
	}
}

func TestMessageSendPolicyRejectsDirectMessageAndExplicitTaskRequest(t *testing.T) {
	ctx := context.Background()
	svc, db := newServiceWithTeamConfig(t, `
team_id: communication-policy-test
version: 1
communication:
  allow_direct_message: false
  allow_followup_task: true
  task_requires_contract: true
agents:
  - id: builder
    runtime_profile: external-debug
    cli_backend: codex
  - id: coordinator
    runtime_profile: external-debug
    cli_backend: codex
`)
	work := addAndClaim(t, ctx, svc, "builder")
	beforeMessages := countRows(t, ctx, db, "messages")
	beforeEnvelopes := countRows(t, ctx, db, "agent_communication_envelopes")
	beforeMailboxes := countRows(t, ctx, db, "mailbox_items")

	disabled := svc.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          work.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "note",
		Body:             "direct messages are disabled",
	})
	if disabled.Status != capability.StatusRejected || disabled.ErrorCode != "DIRECT_MESSAGE_DISABLED" {
		t.Fatalf("direct disabled response = %+v, want DIRECT_MESSAGE_DISABLED", disabled)
	}
	if countRows(t, ctx, db, "messages") != beforeMessages ||
		countRows(t, ctx, db, "agent_communication_envelopes") != beforeEnvelopes ||
		countRows(t, ctx, db, "mailbox_items") != beforeMailboxes {
		t.Fatalf("direct disabled message wrote durable rows")
	}

	allowedSvc, allowedDB := newServiceWithTeamConfig(t, `
team_id: task-request-policy-test
version: 1
communication:
  allow_direct_message: true
  allow_followup_task: true
  task_requires_contract: true
agents:
  - id: builder
    runtime_profile: external-debug
    cli_backend: codex
  - id: coordinator
    runtime_profile: external-debug
    cli_backend: codex
`)
	allowedWork := addAndClaim(t, ctx, allowedSvc, "builder")
	beforeMessages = countRows(t, ctx, allowedDB, "messages")
	beforeEnvelopes = countRows(t, ctx, allowedDB, "agent_communication_envelopes")
	rejectedTask := allowedSvc.SendMessage(ctx, coordination.SendMessageInput{
		LeaseID:          allowedWork.Lease.ID,
		AgentID:          "builder",
		RecipientAgentID: "coordinator",
		Intent:           "task_request",
		Body:             "explicit task request must use contract.add",
	})
	if rejectedTask.Status != capability.StatusRejected || rejectedTask.ErrorCode != "TASK_REQUIRES_CONTRACT" {
		t.Fatalf("task_request response = %+v, want TASK_REQUIRES_CONTRACT", rejectedTask)
	}
	if countRows(t, ctx, allowedDB, "messages") != beforeMessages ||
		countRows(t, ctx, allowedDB, "agent_communication_envelopes") != beforeEnvelopes {
		t.Fatalf("rejected task_request wrote durable rows")
	}
}

func TestContractWaitDoesNotCompleteContract(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	work := addAndClaim(t, ctx, svc, "coordinator")

	assignment, err := svc.WaitContract(ctx, coordination.WaitContractInput{
		LeaseID:        work.Lease.ID,
		AgentID:        "coordinator",
		Reason:         "waiting for child",
		WaitingForRef:  "contract:child",
		SessionRouteID: "route_parent",
	})
	if err != nil {
		t.Fatalf("contract.wait: %v", err)
	}
	if assignment.State != "waiting" {
		t.Fatalf("assignment state = %s, want waiting", assignment.State)
	}
	if got := contractStatus(t, ctx, db, work.Contract.ID); got != "open" {
		t.Fatalf("contract status after wait = %s, want open", got)
	}
}

func TestContractAddCreatesTaskEnvelopeAssignmentAndMailboxProjection(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)

	add := addContract(t, ctx, svc, "builder")
	if add.EnvelopeID == "" || add.MailboxID == "" {
		t.Fatalf("contract.add = %+v, want envelope and mailbox ids", add)
	}
	if got := envelopeKind(t, ctx, db, add.EnvelopeID); got != "task" {
		t.Fatalf("contract.add envelope kind = %q, want task", got)
	}
	if got := mailboxEnvelopeID(t, ctx, db, add.MailboxID); got != add.EnvelopeID {
		t.Fatalf("task mailbox envelope = %q, want %q", got, add.EnvelopeID)
	}
	if countRowsWhere(t, ctx, db, "work_contracts", "id = '"+add.ContractID+"'") != 1 {
		t.Fatalf("contract row missing for %s", add.ContractID)
	}
	if countRowsWhere(t, ctx, db, "assignments", "id = '"+add.AssignmentID+"' AND contract_id = '"+add.ContractID+"'") != 1 {
		t.Fatalf("assignment row missing for %+v", add)
	}
	payload := eventPayload(t, ctx, db, "contract.created", add.ContractID)
	if payload["assignment_id"] != add.AssignmentID || payload["envelope_id"] != add.EnvelopeID {
		t.Fatalf("contract.created payload = %#v, want assignment and envelope ids", payload)
	}
}

func TestContractAddPolicyRejectsFollowupTaskWithoutDurableRows(t *testing.T) {
	ctx := context.Background()
	svc, db := newServiceWithTeamConfig(t, `
team_id: followup-policy-test
version: 1
communication:
  allow_direct_message: true
  allow_followup_task: false
  task_requires_contract: true
agents:
  - id: coordinator
    runtime_profile: external-debug
    cli_backend: codex
  - id: builder
    runtime_profile: external-debug
    cli_backend: codex
`)
	parent := addAndClaim(t, ctx, svc, "coordinator")
	beforeContracts := countRows(t, ctx, db, "work_contracts")
	beforeAssignments := countRows(t, ctx, db, "assignments")
	beforeEnvelopes := countRows(t, ctx, db, "agent_communication_envelopes")
	beforeMailboxes := countRows(t, ctx, db, "mailbox_items")

	_, err := svc.AddContract(ctx, coordination.AddContractInput{
		IssuerLeaseID: parent.Lease.ID,
		IssuerAgentID: "coordinator",
		Title:         "blocked follow-up",
		Objective:     "must not be created",
		TargetAgentID: "builder",
	})
	if err == nil {
		t.Fatal("contract.add follow-up returned nil error with allow_followup_task=false")
	}
	if countRows(t, ctx, db, "work_contracts") != beforeContracts ||
		countRows(t, ctx, db, "assignments") != beforeAssignments ||
		countRows(t, ctx, db, "agent_communication_envelopes") != beforeEnvelopes ||
		countRows(t, ctx, db, "mailbox_items") != beforeMailboxes {
		t.Fatalf("rejected follow-up task wrote durable rows")
	}
}

func TestContractCompleteRequiresEvidenceThenSucceeds(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	work := addAndClaim(t, ctx, svc, "builder")

	missing := svc.CompleteContract(ctx, coordination.CompleteContractInput{
		LeaseID: work.Lease.ID,
		AgentID: "builder",
	})
	if missing.Status != capability.StatusRejected || missing.ErrorCode != "MISSING_REQUIRED_EVIDENCE" {
		t.Fatalf("complete without evidence = %+v, want missing evidence rejected", missing)
	}
	if got := contractStatus(t, ctx, db, work.Contract.ID); got != "open" {
		t.Fatalf("contract status after rejected complete = %s, want open", got)
	}
	if got := leaseState(t, ctx, db, work.Lease.ID); got != "active" {
		t.Fatalf("lease state after rejected complete = %s, want active", got)
	}

	report, err := svc.SubmitReport(ctx, coordination.SubmitReportInput{
		LeaseID: work.Lease.ID,
		AgentID: "builder",
		Summary: "implemented",
		Content: "details",
	})
	if err != nil {
		t.Fatalf("report.submit: %v", err)
	}
	if got := contractStatus(t, ctx, db, work.Contract.ID); got != "open" {
		t.Fatalf("report.submit changed contract status = %s, want open", got)
	}
	done := svc.CompleteContract(ctx, coordination.CompleteContractInput{
		LeaseID:     work.Lease.ID,
		AgentID:     "builder",
		EvidenceIDs: []string{report.ID},
		Summary:     "done",
	})
	if done.Status != capability.StatusAccepted {
		t.Fatalf("complete with evidence = %+v, want accepted", done)
	}
	if got := contractStatus(t, ctx, db, work.Contract.ID); got != "satisfied" {
		t.Fatalf("contract status = %s, want satisfied", got)
	}
	if got := leaseState(t, ctx, db, work.Lease.ID); got != "released" {
		t.Fatalf("lease state = %s, want released", got)
	}
	if done.Data == nil || done.Data.EnvelopeID == "" {
		t.Fatalf("contract.complete data = %+v, want result envelope id", done.Data)
	}
	payload := eventPayload(t, ctx, db, "contract.satisfied", work.Contract.ID)
	if payload["lease_id"] != work.Lease.ID || payload["envelope_id"] != done.Data.EnvelopeID {
		t.Fatalf("contract.satisfied payload = %#v, want lease and envelope ids", payload)
	}
}

func TestReportSubmitStoresContentAsObjectRefAndContextDoesNotInlineBody(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	work := addAndClaim(t, ctx, svc, "builder")
	secret := "sensitive report body should only be available through object.read"

	report, err := svc.SubmitReport(ctx, coordination.SubmitReportInput{
		LeaseID: work.Lease.ID,
		AgentID: "builder",
		Summary: "implemented",
		Content: secret,
	})
	if err != nil {
		t.Fatalf("report.submit: %v", err)
	}
	if report.ContentRef == "" {
		t.Fatalf("report = %+v, want content_ref", report)
	}
	var contentRef, inlineContent string
	if err := db.QueryRowContext(ctx, `
SELECT COALESCE(content_ref, ''), COALESCE(inline_content, '')
FROM evidence
WHERE id = ?`, report.ID).Scan(&contentRef, &inlineContent); err != nil {
		t.Fatalf("query evidence content refs: %v", err)
	}
	if contentRef != report.ContentRef || inlineContent != "" {
		t.Fatalf("evidence content_ref/inline_content = %q/%q, want ref and empty inline", contentRef, inlineContent)
	}

	context, err := svc.ContractContext(ctx, work.Lease.ID, "builder")
	if err != nil {
		t.Fatalf("contract.context: %v", err)
	}
	rawContext, err := json.Marshal(context)
	if err != nil {
		t.Fatalf("marshal contract context: %v", err)
	}
	if bytes.Contains(rawContext, []byte(secret)) {
		t.Fatalf("contract context leaked report content: %s", rawContext)
	}
	if len(context.Evidence) != 1 || context.Evidence[0].ContentRef != report.ContentRef {
		t.Fatalf("context evidence = %+v, want content_ref %s", context.Evidence, report.ContentRef)
	}

	read := svc.ObjectStore().Read(ctx, agentSubject("builder"), report.ContentRef)
	if read.Status != capability.StatusAccepted || read.Data == nil || read.Data.Content != secret {
		t.Fatalf("object.read = %+v, want report body", read)
	}
}

func TestObjectInspectReadCapabilitiesThroughAdapters(t *testing.T) {
	for _, adapter := range []string{"http", "coordlink"} {
		t.Run(adapter, func(t *testing.T) {
			ctx := context.Background()
			svc, _ := newService(t)
			dispatcher := newCoordinationDispatcher(t, svc)
			work := addAndClaim(t, ctx, svc, "builder")
			report := callAcceptedData[coordination.Evidence](t, adapter, dispatcher, capability.Call{
				CapabilityName: "report.submit",
				Subject:        agentSubject("builder"),
				Scope:          mustRaw(t, map[string]any{"lease_id": work.Lease.ID}),
				Input: mustRaw(t, map[string]any{
					"summary": "implemented",
					"content": "adapter-visible only through object.read",
				}),
			})
			if report.ContentRef == "" {
				t.Fatalf("report = %+v, want content_ref", report)
			}

			inspect := callAcceptedData[objects.ObjectMeta](t, adapter, dispatcher, capability.Call{
				CapabilityName: "object.inspect",
				Subject:        agentSubject("builder"),
				Input:          mustRaw(t, map[string]any{"object_ref": report.ContentRef}),
			})
			if inspect.Ref != report.ContentRef || inspect.SizeBytes == 0 || inspect.Checksum == "" {
				t.Fatalf("object.inspect = %+v, want metadata for report ref", inspect)
			}
			read := callAcceptedData[objects.ObjectContent](t, adapter, dispatcher, capability.Call{
				CapabilityName: "object.read",
				Subject:        agentSubject("builder"),
				Input:          mustRaw(t, map[string]any{"object_ref": report.ContentRef}),
			})
			if read.Content != "adapter-visible only through object.read" {
				t.Fatalf("object.read content = %q", read.Content)
			}

			denied := callPublic(t, adapter, dispatcher, capability.Call{
				CapabilityName: "object.read",
				Subject:        agentSubject("intruder"),
				Input:          mustRaw(t, map[string]any{"object_ref": report.ContentRef}),
			})
			if denied.response.Status != capability.StatusRejected || denied.response.ErrorCode != "OBJECT_ACCESS_DENIED" {
				t.Fatalf("intruder object.read = %+v, want OBJECT_ACCESS_DENIED", denied.response)
			}
			if denied.httpStatus != http.StatusBadRequest {
				t.Fatalf("intruder object.read http status = %d, want 400", denied.httpStatus)
			}
			missing := callPublic(t, adapter, dispatcher, capability.Call{
				CapabilityName: "object.inspect",
				Subject:        agentSubject("builder"),
				Input:          mustRaw(t, map[string]any{"object_ref": "obj_sha256_missing"}),
			})
			if missing.response.Status != capability.StatusRejected || missing.response.ErrorCode != "OBJECT_NOT_FOUND" {
				t.Fatalf("missing object.inspect = %+v, want OBJECT_NOT_FOUND", missing.response)
			}
			if missing.httpStatus != http.StatusBadRequest {
				t.Fatalf("missing object.inspect http status = %d, want 400", missing.httpStatus)
			}
		})
	}
}

func TestObjectCapabilitiesRequireTeamConfigBindingThroughAdapters(t *testing.T) {
	for _, adapter := range []string{"http", "coordlink"} {
		t.Run(adapter, func(t *testing.T) {
			ctx := context.Background()
			svc, _ := newService(t)
			dispatcher := newCoordinationDispatcherWithConfig(t, svc, teamconfig.Config{
				TeamID:  "coordination-test",
				Version: 1,
				Agents: []teamconfig.AgentConfig{
					{
						ID:             "builder",
						RuntimeProfile: "external-debug",
						CLIBackend:     "codex",
						Capabilities:   capabilitiesWithout(step4Capabilities(), "object.inspect", "object.read"),
					},
					{
						ID:             "coordinator",
						RuntimeProfile: "external-debug",
						CLIBackend:     "codex",
						Capabilities:   step4Capabilities(),
					},
				},
			})
			work := addAndClaim(t, ctx, svc, "builder")
			secret := "builder owned object body must not leak through unauthorized object calls"
			report, err := svc.SubmitReport(ctx, coordination.SubmitReportInput{
				LeaseID: work.Lease.ID,
				AgentID: "builder",
				Summary: "implemented",
				Content: secret,
			})
			if err != nil {
				t.Fatalf("report.submit: %v", err)
			}
			if report.ContentRef == "" {
				t.Fatalf("report = %+v, want content_ref", report)
			}

			for _, capabilityName := range []string{"object.inspect", "object.read"} {
				resp := callPublic(t, adapter, dispatcher, capability.Call{
					CapabilityName: capabilityName,
					Subject:        agentSubject("builder"),
					Input:          mustRaw(t, map[string]any{"object_ref": report.ContentRef}),
				})
				if resp.response.Status != capability.StatusRejected || resp.response.ErrorCode != "UNAUTHORIZED_CAPABILITY_CALL" {
					t.Fatalf("%s unbound response = %+v, want UNAUTHORIZED_CAPABILITY_CALL", capabilityName, resp.response)
				}
				if resp.httpStatus != http.StatusBadRequest {
					t.Fatalf("%s unbound http status = %d, want 400", capabilityName, resp.httpStatus)
				}
				if resp.response.Data != nil {
					t.Fatalf("%s unbound response returned data: %+v", capabilityName, resp.response)
				}
				raw, err := json.Marshal(resp.response)
				if err != nil {
					t.Fatalf("marshal %s rejected response: %v", capabilityName, err)
				}
				if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte(report.ContentRef)) {
					t.Fatalf("%s unbound response leaked object metadata/content: %s", capabilityName, raw)
				}
			}

			for _, capabilityName := range []string{"object.inspect", "object.read"} {
				missing := callPublic(t, adapter, dispatcher, capability.Call{
					CapabilityName: capabilityName,
					Subject:        agentSubject("coordinator"),
					Input:          mustRaw(t, map[string]any{"object_ref": "obj_sha256_missing"}),
				})
				if missing.response.Status != capability.StatusRejected || missing.response.ErrorCode != "OBJECT_NOT_FOUND" {
					t.Fatalf("%s authorized missing response = %+v, want OBJECT_NOT_FOUND", capabilityName, missing.response)
				}
				if missing.httpStatus != http.StatusBadRequest {
					t.Fatalf("%s authorized missing http status = %d, want 400", capabilityName, missing.httpStatus)
				}
				if missing.response.Data != nil {
					t.Fatalf("%s authorized missing response returned data: %+v", capabilityName, missing.response)
				}
			}
		})
	}
}

func TestAssignmentNextCreatesOneActiveLeaseAndEnforcesScope(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	add := addContract(t, ctx, svc, "builder")

	first, err := svc.AssignmentNext(ctx, coordination.AssignmentNextInput{AgentID: "builder", LeaseFor: time.Hour})
	if err != nil {
		t.Fatalf("assignment.next first: %v", err)
	}
	second, err := svc.AssignmentNext(ctx, coordination.AssignmentNextInput{AgentID: "builder", LeaseFor: time.Hour})
	if err != nil {
		t.Fatalf("assignment.next second: %v", err)
	}
	if first.Lease.ID != second.Lease.ID {
		t.Fatalf("duplicate active lease: first=%s second=%s", first.Lease.ID, second.Lease.ID)
	}
	if active := countActiveLeases(t, ctx, db, add.AssignmentID); active != 1 {
		t.Fatalf("active leases = %d, want 1", active)
	}

	if _, err := svc.CurrentContract(ctx, first.Lease.ID, "intruder"); err == nil {
		t.Fatal("intruder read contract through another agent lease")
	}
	wrongAgentComplete := svc.CompleteContract(ctx, coordination.CompleteContractInput{
		LeaseID: first.Lease.ID,
		AgentID: "intruder",
	})
	if wrongAgentComplete.Status != capability.StatusError {
		t.Fatalf("wrong agent complete = %+v, want typed error", wrongAgentComplete)
	}
}

func TestChildCompletionCreatesPublisherMailboxAndResolveRequiresDurableFollowup(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	parent := addAndClaim(t, ctx, svc, "coordinator")
	if _, err := svc.WaitContract(ctx, coordination.WaitContractInput{
		LeaseID:        parent.Lease.ID,
		AgentID:        "coordinator",
		Reason:         "waiting for builder",
		WaitingForRef:  "child",
		SessionRouteID: "route_parent",
	}); err != nil {
		t.Fatalf("parent wait: %v", err)
	}
	childAdd, err := svc.AddContract(ctx, coordination.AddContractInput{
		IssuerLeaseID: parent.Lease.ID,
		IssuerAgentID: "coordinator",
		Title:         "child task",
		Objective:     "finish child",
		TargetAgentID: "builder",
	})
	if err != nil {
		t.Fatalf("child contract.add: %v", err)
	}
	child, err := svc.AssignmentNext(ctx, coordination.AssignmentNextInput{AgentID: "builder", LeaseFor: time.Hour})
	if err != nil {
		t.Fatalf("builder assignment.next: %v", err)
	}
	if child.Contract.ID != childAdd.ContractID {
		t.Fatalf("claimed child contract = %s, want %s", child.Contract.ID, childAdd.ContractID)
	}
	report, err := svc.SubmitReport(ctx, coordination.SubmitReportInput{
		LeaseID: child.Lease.ID,
		AgentID: "builder",
		Summary: "child done",
		Content: "done",
	})
	if err != nil {
		t.Fatalf("child report.submit: %v", err)
	}
	done := svc.CompleteContract(ctx, coordination.CompleteContractInput{
		LeaseID:     child.Lease.ID,
		AgentID:     "builder",
		EvidenceIDs: []string{report.ID},
	})
	if done.Status != capability.StatusAccepted {
		t.Fatalf("child complete = %+v", done)
	}

	mailbox, err := svc.MailboxList(ctx, "coordinator")
	if err != nil {
		t.Fatalf("mailbox.list: %v", err)
	}
	if len(mailbox) != 1 {
		t.Fatalf("coordinator mailbox count = %d, want 1", len(mailbox))
	}
	item := mailbox[0]
	if item.Reason != "child_completed" || item.ContractID != parent.Contract.ID || item.SessionRouteID != "route_parent" {
		t.Fatalf("child_completed mailbox = %+v", item)
	}
	if item.EnvelopeID == "" || item.EnvelopeKind != "result" {
		t.Fatalf("child_completed mailbox envelope projection = %+v, want result envelope", item)
	}
	read := svc.ReadCommunication(ctx, coordination.CommunicationReadInput{
		AgentID:    "coordinator",
		EnvelopeID: item.EnvelopeID,
	})
	if read.Status != capability.StatusAccepted || read.Data == nil || read.Data.Kind != "result" || read.Data.BodyInline == "" {
		t.Fatalf("coordinator communication.read = %+v, want result envelope body", read)
	}
	intruder := svc.ReadCommunication(ctx, coordination.CommunicationReadInput{
		AgentID:    "intruder",
		EnvelopeID: item.EnvelopeID,
	})
	if intruder.Status != capability.StatusRejected || intruder.ErrorCode != "COMMUNICATION_ACCESS_DENIED" || intruder.Data != nil {
		t.Fatalf("intruder communication.read = %+v, want fail-closed rejected without data", intruder)
	}
	rawDenied, err := json.Marshal(intruder)
	if err != nil {
		t.Fatalf("marshal denied communication.read: %v", err)
	}
	if bytes.Contains(rawDenied, []byte(read.Data.BodyInline)) || bytes.Contains(rawDenied, []byte(item.EnvelopeID)) {
		t.Fatalf("denied communication.read leaked envelope content/id: %s", rawDenied)
	}
	if _, err := svc.MailboxGet(ctx, "builder", item.ID); err == nil {
		t.Fatal("builder read coordinator mailbox")
	}

	rejected := svc.MailboxResolve(ctx, coordination.ResolveMailboxInput{
		AgentID:   "coordinator",
		MailboxID: item.ID,
	})
	if rejected.Status != capability.StatusRejected || rejected.ErrorCode != "MISSING_MAILBOX_FOLLOWUP" {
		t.Fatalf("resolve without followup = %+v", rejected)
	}
	accepted := svc.MailboxResolve(ctx, coordination.ResolveMailboxInput{
		AgentID:     "coordinator",
		MailboxID:   item.ID,
		FollowupRef: "message:ack",
	})
	if accepted.Status != capability.StatusAccepted {
		t.Fatalf("resolve with followup = %+v", accepted)
	}
	got, err := svc.MailboxGet(ctx, "coordinator", item.ID)
	if err != nil {
		t.Fatalf("get resolved mailbox: %v", err)
	}
	if got.State != "resolved" || got.FollowupRef != "message:ack" {
		t.Fatalf("resolved mailbox = %+v", got)
	}
}

func TestCoordinationCapabilitiesPreserveRejectedEvidenceThroughAdapters(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	dispatcher := newCoordinationDispatcher(t, svc)
	work := addAndClaim(t, ctx, svc, "builder")

	call := capability.Call{
		CapabilityName: "contract.complete",
		Subject:        agentSubject("builder"),
		Scope:          mustRaw(t, map[string]any{"lease_id": work.Lease.ID}),
		Input:          mustRaw(t, map[string]any{"summary": "done"}),
	}
	httpEnvelope, httpStatus := callHTTP(t, dispatcher, call)
	coordEnvelope := envelope(t, coordlink.New(dispatcher).Call(ctx, call))

	if httpStatus != http.StatusBadRequest {
		t.Fatalf("http status = %d, want 400", httpStatus)
	}
	if !jsonEqual(httpEnvelope, coordEnvelope) {
		t.Fatalf("adapter rejected envelopes differ\nhttp:     %#v\ncoordlink:%#v", httpEnvelope, coordEnvelope)
	}
	if httpEnvelope["status"] != "rejected" || httpEnvelope["error_code"] != "MISSING_REQUIRED_EVIDENCE" {
		t.Fatalf("contract.complete envelope = %#v, want missing-evidence rejected", httpEnvelope)
	}
	canonicalIDs := objectField(t, httpEnvelope, "canonical_ids")
	if canonicalIDs["contract_id"] != work.Contract.ID || canonicalIDs["lease_id"] != work.Lease.ID {
		t.Fatalf("canonical ids = %#v, want contract and lease ids", canonicalIDs)
	}
	if got := contractStatus(t, ctx, db, work.Contract.ID); got != "open" {
		t.Fatalf("rejected adapter complete changed contract status = %s, want open", got)
	}
	if got := leaseState(t, ctx, db, work.Lease.ID); got != "active" {
		t.Fatalf("rejected adapter complete changed lease state = %s, want active", got)
	}
}

func TestCoordinationCapabilitiesMessageSendThroughHTTPDoesNotChangeContractStatus(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t)
	dispatcher := newCoordinationDispatcher(t, svc)
	work := addAndClaim(t, ctx, svc, "builder")

	call := capability.Call{
		CapabilityName: "message.send",
		Subject:        agentSubject("builder"),
		Input: mustRaw(t, map[string]any{
			"lease_id":           work.Lease.ID,
			"recipient_agent_id": "coordinator",
			"intent":             "note",
			"body":               "status update only",
		}),
	}
	httpEnvelope, httpStatus := callHTTP(t, dispatcher, call)
	if httpStatus != http.StatusOK {
		t.Fatalf("http status = %d, want 200: %#v", httpStatus, httpEnvelope)
	}
	if httpEnvelope["status"] != "accepted" || httpEnvelope["ok"] != true {
		t.Fatalf("message.send envelope = %#v, want accepted", httpEnvelope)
	}
	if got := contractStatus(t, ctx, db, work.Contract.ID); got != "open" {
		t.Fatalf("message.send through adapter changed contract status = %s, want open", got)
	}
	if countRows(t, ctx, db, "messages") != 1 {
		t.Fatalf("messages row count = %d, want 1", countRows(t, ctx, db, "messages"))
	}
}

func TestCoordinationPublicChildContractMailboxLoopThroughAdapters(t *testing.T) {
	for _, adapter := range []string{"http", "coordlink"} {
		t.Run(adapter, func(t *testing.T) {
			ctx := context.Background()
			svc, db := newService(t)
			dispatcher := newCoordinationDispatcher(t, svc)
			parentAdd := addContract(t, ctx, svc, "coordinator")

			parentClaim := callAcceptedData[coordination.AssignmentNextResult](t, adapter, dispatcher, capability.Call{
				CapabilityName: "assignment.next",
				Subject:        agentSubject("coordinator"),
			})
			if parentClaim.Contract.ID != parentAdd.ContractID {
				t.Fatalf("parent claim contract = %s, want %s", parentClaim.Contract.ID, parentAdd.ContractID)
			}

			waited := callAcceptedData[coordination.Assignment](t, adapter, dispatcher, capability.Call{
				CapabilityName: "contract.wait",
				Subject:        agentSubject("coordinator"),
				Scope:          mustRaw(t, map[string]any{"lease_id": parentClaim.Lease.ID}),
				Input: mustRaw(t, map[string]any{
					"reason":           "waiting for child",
					"waiting_for_ref":  "contract:child",
					"session_route_id": "route_parent",
				}),
			})
			if waited.State != "waiting" || waited.SessionRouteID != "route_parent" {
				t.Fatalf("waited assignment = %+v, want waiting with route_parent", waited)
			}
			if got := contractStatus(t, ctx, db, parentClaim.Contract.ID); got != "open" {
				t.Fatalf("parent status after public wait = %s, want open", got)
			}

			childAdd := callAcceptedData[coordination.AddContractResult](t, adapter, dispatcher, capability.Call{
				CapabilityName: "contract.add",
				Subject:        agentSubject("coordinator"),
				Scope:          mustRaw(t, map[string]any{"lease_id": parentClaim.Lease.ID}),
				Input: mustRaw(t, map[string]any{
					"title":           "child task",
					"objective":       "finish child",
					"target_agent_id": "builder",
				}),
			})
			issuerAgent, issuerContract := contractIssuer(t, ctx, db, childAdd.ContractID)
			if issuerAgent != "coordinator" || issuerContract != parentClaim.Contract.ID {
				t.Fatalf("child issuer = agent:%s contract:%s, want coordinator/%s", issuerAgent, issuerContract, parentClaim.Contract.ID)
			}

			childClaim := callAcceptedData[coordination.AssignmentNextResult](t, adapter, dispatcher, capability.Call{
				CapabilityName: "assignment.next",
				Subject:        agentSubject("builder"),
			})
			if childClaim.Contract.ID != childAdd.ContractID {
				t.Fatalf("child claim contract = %s, want %s", childClaim.Contract.ID, childAdd.ContractID)
			}
			report := callAcceptedData[coordination.Evidence](t, adapter, dispatcher, capability.Call{
				CapabilityName: "report.submit",
				Subject:        agentSubject("builder"),
				Scope:          mustRaw(t, map[string]any{"lease_id": childClaim.Lease.ID}),
				Input: mustRaw(t, map[string]any{
					"summary": "child done",
					"content": "done",
				}),
			})
			completed := callAcceptedData[coordination.CompleteContractResult](t, adapter, dispatcher, capability.Call{
				CapabilityName: "contract.complete",
				Subject:        agentSubject("builder"),
				Scope:          mustRaw(t, map[string]any{"lease_id": childClaim.Lease.ID}),
				Input: mustRaw(t, map[string]any{
					"evidence_ids": []string{report.ID},
					"summary":      "done",
				}),
			})
			if completed.ContractID != childAdd.ContractID || completed.Status != "satisfied" {
				t.Fatalf("child complete result = %+v", completed)
			}

			mailbox := callAcceptedData[[]coordination.MailboxItem](t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.list",
				Subject:        agentSubject("coordinator"),
			})
			if len(mailbox) != 1 {
				t.Fatalf("publisher mailbox count = %d, want 1: %+v", len(mailbox), mailbox)
			}
			item := mailbox[0]
			if item.Reason != "child_completed" || item.ContractID != parentClaim.Contract.ID || item.SessionRouteID != "route_parent" {
				t.Fatalf("publisher mailbox = %+v, want child_completed for parent route", item)
			}
			gotItem := callAcceptedData[coordination.MailboxItem](t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.get",
				Subject:        agentSubject("coordinator"),
				Input:          mustRaw(t, map[string]any{"mailbox_id": item.ID}),
			})
			if gotItem.ID != item.ID {
				t.Fatalf("mailbox.get id = %s, want %s", gotItem.ID, item.ID)
			}
			rejected := callPublic(t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.resolve",
				Subject:        agentSubject("coordinator"),
				Input:          mustRaw(t, map[string]any{"mailbox_id": item.ID}),
			})
			if rejected.response.Status != capability.StatusRejected || rejected.response.ErrorCode != "MISSING_MAILBOX_FOLLOWUP" {
				t.Fatalf("resolve without followup = %+v, want MISSING_MAILBOX_FOLLOWUP", rejected.response)
			}
			resolved := callAcceptedData[coordination.MailboxItem](t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.resolve",
				Subject:        agentSubject("coordinator"),
				Input: mustRaw(t, map[string]any{
					"mailbox_id":   item.ID,
					"followup_ref": "message:ack",
				}),
			})
			if resolved.State != "resolved" || resolved.FollowupRef != "message:ack" {
				t.Fatalf("resolved mailbox = %+v", resolved)
			}
			state, followup := mailboxState(t, ctx, db, item.ID)
			if state != "resolved" || followup != "message:ack" {
				t.Fatalf("durable mailbox state/followup = %s/%s, want resolved/message:ack", state, followup)
			}
			remaining := callAcceptedData[[]coordination.MailboxItem](t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.list",
				Subject:        agentSubject("coordinator"),
			})
			if len(remaining) != 0 {
				t.Fatalf("resolved mailbox still listed as pending: %+v", remaining)
			}
		})
	}
}

func TestCoordinationPublicRejectsWrongAgentLeaseReuseThroughAdapters(t *testing.T) {
	cases := []struct {
		name       string
		capability string
		input      map[string]any
	}{
		{name: "current", capability: "contract.current"},
		{name: "context", capability: "contract.context"},
		{name: "wait", capability: "contract.wait", input: map[string]any{"reason": "steal wait", "session_route_id": "route_intruder"}},
		{name: "complete", capability: "contract.complete", input: map[string]any{"summary": "steal complete"}},
		{name: "message", capability: "message.send", input: map[string]any{"intent": "note", "body": "steal message"}},
		{name: "report", capability: "report.submit", input: map[string]any{"summary": "steal report", "content": "wrong"}},
	}
	for _, adapter := range []string{"http", "coordlink"} {
		for _, tc := range cases {
			t.Run(adapter+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				svc, db := newService(t)
				dispatcher := newCoordinationDispatcher(t, svc)
				work := addAndClaim(t, ctx, svc, "builder")
				beforeAssignment := assignmentState(t, ctx, db, work.Lease.AssignmentID)
				beforeMessages := countRows(t, ctx, db, "messages")
				beforeEvidence := countRows(t, ctx, db, "evidence")
				beforeMailboxes := countRows(t, ctx, db, "mailbox_items")

				resp := callPublic(t, adapter, dispatcher, capability.Call{
					CapabilityName: tc.capability,
					Subject:        agentSubject("intruder"),
					Scope:          mustRaw(t, map[string]any{"lease_id": work.Lease.ID}),
					Input:          mustRaw(t, tc.input),
				})
				if resp.response.Status != capability.StatusError {
					t.Fatalf("%s wrong-agent response = %+v, want typed error", tc.capability, resp.response)
				}
				if resp.httpStatus != http.StatusInternalServerError {
					t.Fatalf("%s status = %d, want 500-equivalent", tc.capability, resp.httpStatus)
				}
				if got := contractStatus(t, ctx, db, work.Contract.ID); got != "open" {
					t.Fatalf("wrong-agent %s changed contract status = %s", tc.capability, got)
				}
				if got := leaseState(t, ctx, db, work.Lease.ID); got != "active" {
					t.Fatalf("wrong-agent %s changed lease state = %s", tc.capability, got)
				}
				if got := assignmentState(t, ctx, db, work.Lease.AssignmentID); got != beforeAssignment {
					t.Fatalf("wrong-agent %s changed assignment state = %s, want %s", tc.capability, got, beforeAssignment)
				}
				if countRows(t, ctx, db, "messages") != beforeMessages {
					t.Fatalf("wrong-agent %s wrote message", tc.capability)
				}
				if countRows(t, ctx, db, "evidence") != beforeEvidence {
					t.Fatalf("wrong-agent %s wrote evidence", tc.capability)
				}
				if countRows(t, ctx, db, "mailbox_items") != beforeMailboxes {
					t.Fatalf("wrong-agent %s wrote mailbox", tc.capability)
				}
			})
		}
	}
}

func TestCoordinationPublicMailboxOwnershipAndFollowupThroughAdapters(t *testing.T) {
	for _, adapter := range []string{"http", "coordlink"} {
		t.Run(adapter, func(t *testing.T) {
			ctx := context.Background()
			svc, db := newService(t)
			dispatcher := newCoordinationDispatcher(t, svc)
			work := addAndClaim(t, ctx, svc, "builder")

			sent := callAcceptedData[coordination.SendMessageResult](t, adapter, dispatcher, capability.Call{
				CapabilityName: "message.send",
				Subject:        agentSubject("builder"),
				Scope:          mustRaw(t, map[string]any{"lease_id": work.Lease.ID}),
				Input: mustRaw(t, map[string]any{
					"recipient_agent_id": "coordinator",
					"intent":             "question",
					"body":               "please review",
				}),
			})
			if sent.MailboxID == "" {
				t.Fatalf("message.send did not create mailbox: %+v", sent)
			}

			builderList := callAcceptedData[[]coordination.MailboxItem](t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.list",
				Subject:        agentSubject("builder"),
			})
			if len(builderList) != 0 {
				t.Fatalf("builder can list coordinator mailbox: %+v", builderList)
			}
			wrongGet := callPublic(t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.get",
				Subject:        agentSubject("builder"),
				Input:          mustRaw(t, map[string]any{"mailbox_id": sent.MailboxID}),
			})
			if wrongGet.response.Status != capability.StatusError {
				t.Fatalf("wrong-owner get = %+v, want error", wrongGet.response)
			}
			wrongResolve := callPublic(t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.resolve",
				Subject:        agentSubject("builder"),
				Input: mustRaw(t, map[string]any{
					"mailbox_id":   sent.MailboxID,
					"followup_ref": "message:wrong-owner",
				}),
			})
			if wrongResolve.response.Status != capability.StatusError {
				t.Fatalf("wrong-owner resolve = %+v, want error", wrongResolve.response)
			}
			state, followup := mailboxState(t, ctx, db, sent.MailboxID)
			if state != "pending" || followup != "" {
				t.Fatalf("wrong-owner resolve changed mailbox = %s/%s, want pending/empty", state, followup)
			}

			missingFollowup := callPublic(t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.resolve",
				Subject:        agentSubject("coordinator"),
				Input:          mustRaw(t, map[string]any{"mailbox_id": sent.MailboxID}),
			})
			if missingFollowup.response.Status != capability.StatusRejected || missingFollowup.response.ErrorCode != "MISSING_MAILBOX_FOLLOWUP" {
				t.Fatalf("missing-followup resolve = %+v, want repairable rejected", missingFollowup.response)
			}
			state, followup = mailboxState(t, ctx, db, sent.MailboxID)
			if state != "pending" || followup != "" {
				t.Fatalf("missing-followup resolve changed mailbox = %s/%s, want pending/empty", state, followup)
			}

			resolved := callAcceptedData[coordination.MailboxItem](t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.resolve",
				Subject:        agentSubject("coordinator"),
				Input: mustRaw(t, map[string]any{
					"mailbox_id":   sent.MailboxID,
					"followup_ref": "message:reply",
				}),
			})
			if resolved.State != "resolved" || resolved.FollowupRef != "message:reply" {
				t.Fatalf("resolved mailbox = %+v", resolved)
			}
			state, followup = mailboxState(t, ctx, db, sent.MailboxID)
			if state != "resolved" || followup != "message:reply" {
				t.Fatalf("durable resolved mailbox = %s/%s, want resolved/message:reply", state, followup)
			}
			coordinatorList := callAcceptedData[[]coordination.MailboxItem](t, adapter, dispatcher, capability.Call{
				CapabilityName: "mailbox.list",
				Subject:        agentSubject("coordinator"),
			})
			if len(coordinatorList) != 0 {
				t.Fatalf("resolved mailbox still in pending list: %+v", coordinatorList)
			}
		})
	}
}

func TestCoordinationPublicAssignmentNextAndWatchThroughAdapters(t *testing.T) {
	for _, adapter := range []string{"http", "coordlink"} {
		t.Run(adapter, func(t *testing.T) {
			ctx := context.Background()
			svc, db := newService(t)
			dispatcher := newCoordinationDispatcher(t, svc)
			builderAdd := addContract(t, ctx, svc, "builder")
			coordinatorAdd := addContract(t, ctx, svc, "coordinator")

			watched := callAcceptedData[[]coordination.Assignment](t, adapter, dispatcher, capability.Call{
				CapabilityName: "assignment.watch",
				Subject:        agentSubject("builder"),
			})
			if len(watched) != 1 || watched[0].ID != builderAdd.AssignmentID {
				t.Fatalf("builder watch = %+v, want only builder assignment %s", watched, builderAdd.AssignmentID)
			}
			if watched[0].ID == coordinatorAdd.AssignmentID {
				t.Fatalf("builder watch exposed coordinator assignment: %+v", watched)
			}

			first := callAcceptedData[coordination.AssignmentNextResult](t, adapter, dispatcher, capability.Call{
				CapabilityName: "assignment.next",
				Subject:        agentSubject("builder"),
			})
			second := callAcceptedData[coordination.AssignmentNextResult](t, adapter, dispatcher, capability.Call{
				CapabilityName: "assignment.next",
				Subject:        agentSubject("builder"),
			})
			if first.Idle || second.Idle {
				t.Fatalf("assignment.next returned idle: first=%+v second=%+v", first, second)
			}
			if first.Assignment.ID != builderAdd.AssignmentID || second.Assignment.ID != builderAdd.AssignmentID {
				t.Fatalf("claimed assignments = %s/%s, want %s", first.Assignment.ID, second.Assignment.ID, builderAdd.AssignmentID)
			}
			if first.Lease.ID != second.Lease.ID {
				t.Fatalf("duplicate active lease through public next: first=%s second=%s", first.Lease.ID, second.Lease.ID)
			}
			if active := countActiveLeases(t, ctx, db, builderAdd.AssignmentID); active != 1 {
				t.Fatalf("active builder leases = %d, want 1", active)
			}
			afterClaim := callAcceptedData[[]coordination.Assignment](t, adapter, dispatcher, capability.Call{
				CapabilityName: "assignment.watch",
				Subject:        agentSubject("builder"),
			})
			if len(afterClaim) != 0 {
				t.Fatalf("builder watch after claim = %+v, want no queued assignments", afterClaim)
			}
		})
	}
}

type claimedWork struct {
	Add      coordination.AddContractResult
	Contract coordination.Contract
	Lease    coordination.Lease
}

func addAndClaim(t *testing.T, ctx context.Context, svc *coordination.Service, agentID string) claimedWork {
	t.Helper()
	add := addContract(t, ctx, svc, agentID)
	next, err := svc.AssignmentNext(ctx, coordination.AssignmentNextInput{AgentID: agentID, LeaseFor: time.Hour})
	if err != nil {
		t.Fatalf("assignment.next: %v", err)
	}
	if next.Idle {
		t.Fatal("assignment.next returned idle")
	}
	return claimedWork{Add: add, Contract: next.Contract, Lease: next.Lease}
}

func addContract(t *testing.T, ctx context.Context, svc *coordination.Service, targetAgent string) coordination.AddContractResult {
	t.Helper()
	add, err := svc.AddContract(ctx, coordination.AddContractInput{
		IssuerAgentID: "operator",
		Title:         "task for " + targetAgent,
		Objective:     "complete work",
		TargetAgentID: targetAgent,
	})
	if err != nil {
		t.Fatalf("contract.add: %v", err)
	}
	return add
}

func newService(t *testing.T) (*coordination.Service, *sql.DB) {
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
	return coordination.NewService(s), db
}

func newServiceWithTeamConfig(t *testing.T, rawYAML string) (*coordination.Service, *sql.DB) {
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
	cfg, err := teamconfig.NewRepository(s).SaveYAML(context.Background(), []byte(rawYAML))
	if err != nil {
		t.Fatalf("save TeamConfig: %v", err)
	}
	return coordination.NewServiceWithTeam(s, cfg.TeamID, cfg.Version), db
}

func contractStatus(t *testing.T, ctx context.Context, db *sql.DB, contractID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_contracts WHERE id = ?`, contractID).Scan(&status); err != nil {
		t.Fatalf("query contract status: %v", err)
	}
	return status
}

func leaseState(t *testing.T, ctx context.Context, db *sql.DB, leaseID string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, leaseID).Scan(&state); err != nil {
		t.Fatalf("query lease state: %v", err)
	}
	return state
}

func countRows(t *testing.T, ctx context.Context, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func countRowsWhere(t *testing.T, ctx context.Context, db *sql.DB, table, where string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where).Scan(&count); err != nil {
		t.Fatalf("count %s where %s: %v", table, where, err)
	}
	return count
}

func envelopeKind(t *testing.T, ctx context.Context, db *sql.DB, envelopeID string) string {
	t.Helper()
	var kind string
	if err := db.QueryRowContext(ctx, `SELECT kind FROM agent_communication_envelopes WHERE id = ?`, envelopeID).Scan(&kind); err != nil {
		t.Fatalf("query envelope %s kind: %v", envelopeID, err)
	}
	return kind
}

func mailboxEnvelopeID(t *testing.T, ctx context.Context, db *sql.DB, mailboxID string) string {
	t.Helper()
	var envelopeID string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(envelope_id, '') FROM mailbox_items WHERE id = ?`, mailboxID).Scan(&envelopeID); err != nil {
		t.Fatalf("query mailbox %s envelope: %v", mailboxID, err)
	}
	return envelopeID
}

func eventPayload(t *testing.T, ctx context.Context, db *sql.DB, eventType, aggregateID string) map[string]any {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(ctx, `
SELECT payload_json
FROM events
WHERE event_type = ? AND aggregate_id = ?
ORDER BY occurred_at DESC, id DESC
LIMIT 1`, eventType, aggregateID).Scan(&raw); err != nil {
		t.Fatalf("query event %s/%s: %v", eventType, aggregateID, err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode event payload %s/%s: %v", eventType, aggregateID, err)
	}
	return payload
}

func countActiveLeases(t *testing.T, ctx context.Context, db *sql.DB, assignmentID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM leases WHERE assignment_id = ? AND state = 'active'`, assignmentID).Scan(&count); err != nil {
		t.Fatalf("count active leases: %v", err)
	}
	return count
}

func assignmentState(t *testing.T, ctx context.Context, db *sql.DB, assignmentID string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM assignments WHERE id = ?`, assignmentID).Scan(&state); err != nil {
		t.Fatalf("query assignment state: %v", err)
	}
	return state
}

func contractIssuer(t *testing.T, ctx context.Context, db *sql.DB, contractID string) (string, string) {
	t.Helper()
	var issuerAgent, issuerContract string
	if err := db.QueryRowContext(ctx, `
SELECT COALESCE(issuer_agent_id, ''), COALESCE(issuer_contract_id, '')
FROM work_contracts
WHERE id = ?`, contractID).Scan(&issuerAgent, &issuerContract); err != nil {
		t.Fatalf("query contract issuer: %v", err)
	}
	return issuerAgent, issuerContract
}

func mailboxState(t *testing.T, ctx context.Context, db *sql.DB, mailboxID string) (string, string) {
	t.Helper()
	var state, followup string
	if err := db.QueryRowContext(ctx, `
SELECT state, COALESCE(followup_ref, '')
FROM mailbox_items
WHERE id = ?`, mailboxID).Scan(&state, &followup); err != nil {
		t.Fatalf("query mailbox state: %v", err)
	}
	return state, followup
}

func newCoordinationDispatcher(t *testing.T, svc *coordination.Service) *policy.Dispatcher {
	t.Helper()
	return newCoordinationDispatcherWithConfig(t, svc, teamconfig.Config{
		TeamID:  "coordination-test",
		Version: 1,
		Agents: []teamconfig.AgentConfig{
			{
				ID:             "builder",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Capabilities:   step4Capabilities(),
			},
			{
				ID:             "coordinator",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Capabilities:   step4Capabilities(),
			},
			{
				ID:             "intruder",
				RuntimeProfile: "external-debug",
				CLIBackend:     "codex",
				Capabilities:   step4Capabilities(),
			},
		},
	})
}

func newCoordinationDispatcherWithConfig(t *testing.T, svc *coordination.Service, cfg teamconfig.Config) *policy.Dispatcher {
	t.Helper()
	registry := capability.NewRegistry()
	if err := coordination.RegisterCapabilities(registry, svc); err != nil {
		t.Fatalf("register coordination capabilities: %v", err)
	}
	if err := objects.RegisterCapabilities(registry, svc.ObjectStore()); err != nil {
		t.Fatalf("register object capabilities: %v", err)
	}
	return policy.NewDispatcher(cfg, registry)
}

func step4Capabilities() []string {
	return []string{
		"contract.add",
		"contract.current",
		"contract.context",
		"contract.wait",
		"contract.complete",
		"assignment.next",
		"assignment.watch",
		"message.send",
		"communication.read",
		"mailbox.list",
		"mailbox.get",
		"mailbox.resolve",
		"report.submit",
		"object.inspect",
		"object.read",
	}
}

func capabilitiesWithout(capabilities []string, excluded ...string) []string {
	blocked := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		blocked[name] = true
	}
	var out []string
	for _, name := range capabilities {
		if !blocked[name] {
			out = append(out, name)
		}
	}
	return out
}

func agentSubject(agentID string) capability.Subject {
	return capability.Subject{Kind: "agent", ID: agentID, AgentID: agentID}
}

func callHTTP(t *testing.T, dispatcher *policy.Dispatcher, call capability.Call) (map[string]any, int) {
	t.Helper()
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("marshal capability call: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/call", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	httpapi.New(dispatcher).ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode HTTP response: %v\nbody: %s", err, rec.Body.String())
	}
	return out, rec.Code
}

type publicResponse struct {
	response   capability.Response[json.RawMessage]
	httpStatus int
}

func callPublic(t *testing.T, adapter string, dispatcher *policy.Dispatcher, call capability.Call) publicResponse {
	t.Helper()
	switch adapter {
	case "http":
		body, err := json.Marshal(call)
		if err != nil {
			t.Fatalf("marshal capability call: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/call", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		httpapi.New(dispatcher).ServeHTTP(rec, req)
		var response capability.Response[json.RawMessage]
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode HTTP capability response: %v\nbody: %s", err, rec.Body.String())
		}
		return publicResponse{response: response, httpStatus: rec.Code}
	case "coordlink":
		response := coordlink.New(dispatcher).Call(context.Background(), call)
		return publicResponse{response: response, httpStatus: statusForResponse(response)}
	default:
		t.Fatalf("unknown adapter %q", adapter)
		return publicResponse{}
	}
}

func statusForResponse(response capability.Response[json.RawMessage]) int {
	switch response.Status {
	case capability.StatusAccepted:
		return http.StatusOK
	case capability.StatusRejected:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func callAcceptedData[T any](t *testing.T, adapter string, dispatcher *policy.Dispatcher, call capability.Call) T {
	t.Helper()
	result := callPublic(t, adapter, dispatcher, call)
	if result.httpStatus != http.StatusOK || result.response.Status != capability.StatusAccepted || !result.response.OK {
		t.Fatalf("%s %s response = status:%d %+v, want accepted", adapter, call.CapabilityName, result.httpStatus, result.response)
	}
	if result.response.Data == nil {
		t.Fatalf("%s %s accepted response missing data", adapter, call.CapabilityName)
	}
	var out T
	if err := json.Unmarshal(*result.response.Data, &out); err != nil {
		t.Fatalf("decode %s %s data: %v\nraw: %s", adapter, call.CapabilityName, err, string(*result.response.Data))
	}
	return out
}

func envelope(t *testing.T, response capability.Response[json.RawMessage]) map[string]any {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal capability response: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode capability response: %v", err)
	}
	return out
}

func mustRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw JSON: %v", err)
	}
	return raw
}

func objectField(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, object[key])
	}
	return value
}

func jsonEqual(left, right map[string]any) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}
