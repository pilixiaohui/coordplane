package transport

import (
	"encoding/json"
	"net/http"

	"coordplane/internal/core"
)

// Envelope is the stable wire response used by both local transports.
type Envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *core.Error     `json:"error"`
}

func writeResult(w http.ResponseWriter, data any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	raw, marshalErr := json.Marshal(data)
	if marshalErr != nil {
		writeError(w, core.WrapError(core.CodeInternal, "encode response", false, marshalErr))
		return
	}
	writeEnvelope(w, http.StatusOK, Envelope{OK: true, Data: raw})
}

func writeError(w http.ResponseWriter, err error) {
	coreErr := core.AsError(err)
	writeErrorStatus(w, statusForCode(coreErr.Code), coreErr)
}

func writeErrorStatus(w http.ResponseWriter, status int, err *core.Error) {
	writeEnvelope(w, status, Envelope{OK: false, Data: json.RawMessage("null"), Error: err})
}

func writeEnvelope(w http.ResponseWriter, status int, envelope Envelope) {
	if envelope.Data == nil {
		envelope.Data = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope)
}

func statusForCode(code core.ErrorCode) int {
	switch code {
	case core.CodeInvalidArgument:
		return http.StatusBadRequest
	case core.CodeNotFound:
		return http.StatusNotFound
	case core.CodeScopeDenied:
		return http.StatusForbidden
	case core.CodeInvalidState, core.CodeActionInProgress, core.CodeVersionConflict,
		core.CodeStaleRun, core.CodeRunStarting, core.CodeAgentBusy, core.CodeGitDirty,
		core.CodeGitStale, core.CodeIntegrationAgentRequired:
		return http.StatusConflict
	case core.CodeRuntimeUnavailable, core.CodeResumeUnavailable, core.CodeLegacySchemaRebuildRequired:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
