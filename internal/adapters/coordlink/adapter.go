package coordlink

import (
	"context"
	"encoding/json"

	"coordplane/internal/capability"
)

type Dispatcher interface {
	Handle(context.Context, capability.Call) capability.Response[json.RawMessage]
	ListForSubject(context.Context, capability.Subject) capability.Response[json.RawMessage]
}

type Adapter struct {
	dispatcher Dispatcher
}

func New(dispatcher Dispatcher) *Adapter {
	return &Adapter{dispatcher: dispatcher}
}

func (a *Adapter) Call(ctx context.Context, call capability.Call) capability.Response[json.RawMessage] {
	return a.dispatcher.Handle(ctx, call)
}

func (a *Adapter) List(ctx context.Context, subject capability.Subject) capability.Response[json.RawMessage] {
	return a.dispatcher.ListForSubject(ctx, subject)
}
