package events

import "time"

type Event struct {
	ID             string
	TenantID       string
	TraceID        string
	SubjectKind    string
	SubjectID      string
	AgentID        string
	RuntimeID      string
	CapabilityName string
	Type           string
	AggregateType  string
	AggregateID    string
	PayloadJSON    []byte
	OccurredAt     time.Time
}
