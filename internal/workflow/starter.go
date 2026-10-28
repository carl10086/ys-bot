package workflow

import (
	"context"

	"go.temporal.io/sdk/client"
)

// TemporalStarter 把 api.TurnStarter 接到真 Temporal client。
type TemporalStarter struct {
	Client client.Client
}

func (t TemporalStarter) StartTurn(ctx context.Context, workflowID, sessionID string) error {
	_, err := t.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: TaskQueue,
	}, TurnWorkflow, TurnInput{SessionID: sessionID})
	return err
}

func (t TemporalStarter) CancelTurn(ctx context.Context, workflowID string) error {
	return t.Client.CancelWorkflow(ctx, workflowID, "")
}
