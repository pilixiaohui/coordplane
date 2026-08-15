package core

// LaunchSessionContext is the adapter-independent identity used for resume
// compatibility. The daemon supplies workspace semantics (a real path or the
// reserved conversation marker) before calling the adapter.
type LaunchSessionContext struct {
	AdapterID   string
	AgentID     string
	TaskID      string
	WorkspaceID string
}

// ResumePolicy is the adapter capability core needs for the resume decision.
// Core never imports an adapter implementation.
type ResumePolicy struct {
	SupportsResume bool
	Compatible     func(previous, next LaunchSessionContext) bool
}

// LaunchModeSelection is the single one-time launch decision for a Run.
type LaunchModeSelection struct {
	Mode                  string
	ResumedFromRunID      string
	ResumeNativeSessionID string
}

// SelectLaunchMode decides between fresh start and resume before any external
// process is created. Resume requires the previous Run to carry the identical
// non-empty config fingerprint and the adapter to approve the session context;
// every other case fails closed into a fresh start. A previous failed resume
// (RESUME_UNAVAILABLE) remains a start, but keeps the failed Run as the
// fallback source so BeginRunLaunch can emit run.resume_fallback.
func SelectLaunchMode(
	policy ResumePolicy,
	previous Run,
	previousContext, currentContext LaunchSessionContext,
	currentFingerprint string,
) LaunchModeSelection {
	start := LaunchModeSelection{Mode: "start"}
	if previous.ID == "" || !policy.SupportsResume || policy.Compatible == nil {
		return start
	}
	if previousContext.AdapterID != currentContext.AdapterID ||
		previousContext.AgentID != currentContext.AgentID ||
		previousContext.TaskID != currentContext.TaskID ||
		previousContext.WorkspaceID != currentContext.WorkspaceID {
		return start
	}
	if currentFingerprint == "" || previous.ConfigFingerprint == "" ||
		previous.ConfigFingerprint != currentFingerprint {
		return start
	}
	if previous.RuntimeErrorCode == string(CodeResumeUnavailable) {
		return LaunchModeSelection{Mode: "start", ResumedFromRunID: previous.ID}
	}
	if previous.NativeSessionID == "" || !policy.Compatible(previousContext, currentContext) {
		return start
	}
	return LaunchModeSelection{
		Mode: "resume", ResumedFromRunID: previous.ID,
		ResumeNativeSessionID: previous.NativeSessionID,
	}
}
