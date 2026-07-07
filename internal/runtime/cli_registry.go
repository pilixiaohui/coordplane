package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type CLIAdapterRegistration struct {
	Name    string
	Kind    string
	Ready   bool
	Adapter CLIAdapter
}

type CLIAdapterRegistry struct {
	db       *sql.DB
	adapters map[string]CLIAdapter
}

func NewCLIAdapterRegistry(db *sql.DB, registrations []CLIAdapterRegistration) *CLIAdapterRegistry {
	out := &CLIAdapterRegistry{
		db:       db,
		adapters: make(map[string]CLIAdapter),
	}
	for _, registration := range registrations {
		if registration.Name == "" || registration.Adapter == nil || !registration.Ready {
			continue
		}
		out.adapters[registration.Name] = registration.Adapter
	}
	return out
}

func (r *CLIAdapterRegistry) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	adapter, err := r.adapter(req.CLIBackend)
	if err != nil {
		return StartResult{}, err
	}
	return adapter.Start(ctx, req)
}

func (r *CLIAdapterRegistry) Steer(ctx context.Context, req SteerRequest) error {
	adapter, err := r.adapter(req.Route.CLIBackend)
	if err != nil {
		return err
	}
	return adapter.Steer(ctx, req)
}

func (r *CLIAdapterRegistry) Resume(ctx context.Context, req ResumeRequest) error {
	adapter, err := r.adapter(req.Route.CLIBackend)
	if err != nil {
		return err
	}
	resumer, ok := adapter.(resumeAdapter)
	if !ok {
		return fmt.Errorf("session.resume: CLI adapter %q does not support resume", req.Route.CLIBackend)
	}
	return resumer.Resume(ctx, req)
}

func (r *CLIAdapterRegistry) Finish(ctx context.Context, report TerminalReport) error {
	if report.AttemptID == "" {
		return errors.New("cli adapter registry: attempt id is required")
	}
	backend, err := r.backendForAttempt(ctx, report.AttemptID)
	if err != nil {
		return err
	}
	adapter, err := r.adapter(backend)
	if err != nil {
		return err
	}
	return adapter.Finish(ctx, report)
}

func (r *CLIAdapterRegistry) CapabilitiesForBackend(backend string) (CLIAdapterCapabilities, bool) {
	adapter, err := r.adapter(backend)
	if err != nil {
		return CLIAdapterCapabilities{}, false
	}
	provider, ok := adapter.(CLIAdapterCapabilityProvider)
	if !ok {
		return CLIAdapterCapabilities{}, false
	}
	return provider.Capabilities(), true
}

func (r *CLIAdapterRegistry) adapter(name string) (CLIAdapter, error) {
	if name == "" {
		name = "fake"
	}
	if r == nil || len(r.adapters) == 0 {
		return nil, errors.New("cli adapter registry: no adapters registered")
	}
	adapter, ok := r.adapters[name]
	if !ok || adapter == nil {
		return nil, fmt.Errorf("cli adapter registry: CLI backend %q is not registered or not ready", name)
	}
	return adapter, nil
}

func (r *CLIAdapterRegistry) backendForAttempt(ctx context.Context, attemptID string) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("cli adapter registry: database is required")
	}
	var backend string
	if err := r.db.QueryRowContext(ctx, `SELECT cli_backend FROM attempts WHERE id = ?`, attemptID).Scan(&backend); err != nil {
		return "", fmt.Errorf("cli adapter registry: find attempt backend: %w", err)
	}
	return backend, nil
}
