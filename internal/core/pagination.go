package core

const (
	DefaultPageLimit        = 100
	MaximumPageLimit        = 500
	MaximumCompactPageLimit = 100
	MessagePageLimit        = 20
	EventPageLimit          = 20
	StatusSnapshotLimit     = 8

	MaximumMessageBodyBytes     = 64 << 10
	MaximumTaskDescriptionBytes = 256 << 10
	MaximumProgressSummaryBytes = 4 << 10
	MaximumOutcomeTextBytes     = 4 << 10
	MaximumTerminalTextBytes    = 2 << 10
	MaximumEventPayloadBytes    = 32 << 10
)

// NormalizePageLimit applies the public history-page default and maximum.
func NormalizePageLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, NewError(CodeInvalidArgument, "limit must be a non-negative integer", false)
	}
	if limit == 0 {
		return DefaultPageLimit, nil
	}
	if limit > MaximumPageLimit {
		return 0, NewError(CodeInvalidArgument, "limit must not exceed 500", false)
	}
	return limit, nil
}

func NormalizeMessagePageLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, NewError(CodeInvalidArgument, "limit must be a non-negative integer", false)
	}
	if limit == 0 {
		return MessagePageLimit, nil
	}
	if limit > MessagePageLimit {
		return 0, NewError(CodeInvalidArgument, "message limit must not exceed 20", false)
	}
	return limit, nil
}

func NormalizeCompactPageLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, NewError(CodeInvalidArgument, "limit must be a non-negative integer", false)
	}
	if limit == 0 {
		return DefaultPageLimit, nil
	}
	if limit > MaximumCompactPageLimit {
		return 0, NewError(CodeInvalidArgument, "limit must not exceed 100", false)
	}
	return limit, nil
}

func NormalizeEventPageLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, NewError(CodeInvalidArgument, "limit must be a non-negative integer", false)
	}
	if limit == 0 {
		return EventPageLimit, nil
	}
	if limit > EventPageLimit {
		return 0, NewError(CodeInvalidArgument, "event limit must not exceed 20", false)
	}
	return limit, nil
}
