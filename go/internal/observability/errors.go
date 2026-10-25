package observability

import "fmt"

// OrchestratorAckErrorCode mirrors the keys of ORCHESTRATOR_ACK_ERROR_CODES.
type OrchestratorAckErrorCode string

const (
	AckNotOwner          OrchestratorAckErrorCode = "ORCHESTRATOR_NOT_OWNER"
	AckEventInvalidated  OrchestratorAckErrorCode = "ORCHESTRATOR_EVENT_INVALIDATED"
	AckEventFailed       OrchestratorAckErrorCode = "ORCHESTRATOR_EVENT_FAILED"
	AckEventAlreadyAcked OrchestratorAckErrorCode = "ORCHESTRATOR_EVENT_ALREADY_ACKED"
	AckEventNotInScope   OrchestratorAckErrorCode = "ORCHESTRATOR_EVENT_NOT_IN_SCOPE"
	AckEventOutOfOrder   OrchestratorAckErrorCode = "ORCHESTRATOR_EVENT_OUT_OF_ORDER"
	AckOwnerReplaced     OrchestratorAckErrorCode = "ORCHESTRATOR_OWNER_REPLACED"
	AckEventNotFound     OrchestratorAckErrorCode = "ORCHESTRATOR_EVENT_NOT_FOUND"
	AckBusy              OrchestratorAckErrorCode = "ORCHESTRATOR_BUSY"
	AckConnectionLost    OrchestratorAckErrorCode = "ORCHESTRATOR_CONNECTION_LOST"
	AckReconciling       OrchestratorAckErrorCode = "ORCHESTRATOR_RECONCILING"
	AckTimeout           OrchestratorAckErrorCode = "ORCHESTRATOR_ACK_TIMEOUT"
)

// OrchestratorAckErrorCodeRetryable mirrors ORCHESTRATOR_ACK_ERROR_CODES's
// `retryable` flag per code.
var OrchestratorAckErrorCodeRetryable = map[OrchestratorAckErrorCode]bool{
	AckNotOwner:          false,
	AckEventInvalidated:  false,
	AckEventFailed:       false,
	AckEventAlreadyAcked: false,
	AckEventNotInScope:   false,
	AckEventOutOfOrder:   false,
	AckOwnerReplaced:     false,
	AckEventNotFound:     false,
	AckBusy:              true,
	AckConnectionLost:    true,
	AckReconciling:       true,
	AckTimeout:           true,
}

// OrchestratorAckMessages mirrors ORCHESTRATOR_ACK_MESSAGES.
const (
	MsgAckNotOwner    = "Only the current orchestrator can acknowledge notifications"
	MsgAckInvalidated = "orchestrator event is no longer pending (invalidated)"
	MsgAckOutOfOrder  = "Only the next pending orchestrator event can be acknowledged"
)

// OrchestratorAckError mirrors the TS class: an error carrying a code and the
// retryability derived from that code.
type OrchestratorAckError struct {
	Code    OrchestratorAckErrorCode
	Message string
}

// NewOrchestratorAckError mirrors the TS constructor, including the lookup that
// panics on an unknown code (TS: undefined property access).
func NewOrchestratorAckError(code OrchestratorAckErrorCode, message string) *OrchestratorAckError {
	if _, ok := OrchestratorAckErrorCodeRetryable[code]; !ok {
		panic(fmt.Sprintf("unknown ORCHESTRATOR_ACK_ERROR_CODES key: %s", code))
	}
	return &OrchestratorAckError{Code: code, Message: message}
}

func (e *OrchestratorAckError) Error() string { return e.Message }

// Retryable mirrors `this.retryable`.
func (e *OrchestratorAckError) Retryable() bool {
	return OrchestratorAckErrorCodeRetryable[e.Code]
}

// Name mirrors `this.name = "OrchestratorAckError"`.
func (e *OrchestratorAckError) Name() string { return "OrchestratorAckError" }
