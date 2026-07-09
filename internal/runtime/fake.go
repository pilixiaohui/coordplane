package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type FakeCLIAdapter struct {
	mu              sync.Mutex
	starts          []StartRequest
	steers          []SteerRequest
	resumes         []ResumeRequest
	terminalReports []TerminalReport
	nextSession     int
	steerErr        error
}

func NewFakeCLIAdapter() *FakeCLIAdapter {
	return &FakeCLIAdapter{}
}

func (a *FakeCLIAdapter) Capabilities() CLIAdapterCapabilities {
	return CLIAdapterCapabilities{SupportsSameTurnSteer: true}
}

func (a *FakeCLIAdapter) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	select {
	case <-ctx.Done():
		return StartResult{}, ctx.Err()
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextSession++
	a.starts = append(a.starts, cloneStartRequest(req))
	sessionID := req.SessionNativeID
	if sessionID == "" {
		sessionID = fmt.Sprintf("fake-session-%s-%d", req.AgentID, a.nextSession)
	}
	return StartResult{
		SessionNativeID: sessionID,
		TranscriptRef:   fmt.Sprintf("fake-transcript-%s-%d", req.AgentID, a.nextSession),
	}, nil
}

func (a *FakeCLIAdapter) Steer(ctx context.Context, req SteerRequest) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.steerErr != nil {
		return a.steerErr
	}
	a.steers = append(a.steers, req)
	return nil
}

func (a *FakeCLIAdapter) Resume(ctx context.Context, req ResumeRequest) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumes = append(a.resumes, cloneResumeRequest(req))
	return nil
}

func (a *FakeCLIAdapter) Finish(ctx context.Context, report TerminalReport) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.terminalReports = append(a.terminalReports, report)
	return nil
}

func (a *FakeCLIAdapter) Starts() []StartRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]StartRequest, len(a.starts))
	for i, req := range a.starts {
		out[i] = cloneStartRequest(req)
	}
	return out
}

func (a *FakeCLIAdapter) Steers() []SteerRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]SteerRequest, len(a.steers))
	copy(out, a.steers)
	return out
}

func (a *FakeCLIAdapter) Resumes() []ResumeRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ResumeRequest, len(a.resumes))
	for i, req := range a.resumes {
		out[i] = cloneResumeRequest(req)
	}
	return out
}

func (a *FakeCLIAdapter) TerminalReports() []TerminalReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]TerminalReport, len(a.terminalReports))
	copy(out, a.terminalReports)
	return out
}

func (a *FakeCLIAdapter) FailSteer(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if message == "" {
		a.steerErr = nil
		return
	}
	a.steerErr = errors.New(message)
}

func cloneStartRequest(req StartRequest) StartRequest {
	cloned := req
	cloned.Env = make(map[string]string, len(req.Env))
	for key, value := range req.Env {
		cloned.Env[key] = value
	}
	return cloned
}

func cloneResumeRequest(req ResumeRequest) ResumeRequest {
	cloned := req
	cloned.MailboxIDs = append([]string(nil), req.MailboxIDs...)
	cloned.Env = make(map[string]string, len(req.Env))
	for key, value := range req.Env {
		cloned.Env[key] = value
	}
	return cloned
}
