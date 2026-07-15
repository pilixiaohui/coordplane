package core

type requestDedupe struct {
	scope, operation, requestID, inputHash string
}

func (d requestDedupe) replay(tx Transaction) (dedupeResult, bool, error) {
	raw, ok, err := tx.Dedupe(d.scope, d.operation, d.requestID)
	if err != nil || !ok {
		return dedupeResult{}, ok, err
	}
	result, err := decodeDedupe(raw, d.inputHash)
	return result, true, err
}

func (d requestDedupe) record(tx Transaction, id, relatedID, now string) error {
	raw, err := encodeDedupe(id, relatedID, d.inputHash)
	if err != nil {
		return err
	}
	return tx.PutDedupe(d.scope, d.operation, d.requestID, raw, now)
}

type messageInsert struct {
	projectID, taskID, relatedTaskID string
	senderKind, senderID             string
	recipientKind, recipientID       string
	replyTo, body, systemCode        string
	wake                             bool
	maxDeliveries                    int
	idempotencyKey                   string
	actor                            taskMutationActor
	requestID, operationID, now      string
	payload                          string
}

func (s *Service) insertMessage(tx Transaction, input messageInsert) (Message, error) {
	id, err := s.requiredID("msg")
	if err != nil {
		return Message{}, err
	}
	message := Message{
		ID: id, ProjectID: input.projectID, TaskID: input.taskID,
		RelatedTaskID: input.relatedTaskID, SenderKind: input.senderKind, SenderID: input.senderID,
		RecipientKind: input.recipientKind, RecipientID: input.recipientID,
		ReplyToMessageID: input.replyTo, SystemCode: input.systemCode, Body: input.body,
		Wake: input.wake, State: MessagePending, MaxDeliveries: input.maxDeliveries,
		NextDeliveryAt: input.now, IdempotencyKey: input.idempotencyKey,
		Version: 1, CreatedAt: input.now,
	}
	if err := tx.InsertMessage(message); err != nil {
		return Message{}, err
	}
	_, err = tx.AppendEvent(event(input.projectID, "message", id, "message.created",
		input.actor.kind, input.actor.id, input.actor.runID, input.requestID, input.operationID,
		input.payload, input.now))
	return message, err
}
