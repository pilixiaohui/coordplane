package perfobs

const ClientPrefix = "COORDPLANE_PERF_CLIENT "

type Fields struct {
	RequestID   string
	OperationID string
	ProjectID   string
	TaskID      string
	RunID       string
	MessageID   string
}
