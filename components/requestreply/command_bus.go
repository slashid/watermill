package requestreply

import (
	"context"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/pkg/errors"
)

type CommandBus interface {
	SendWithModifiedMessage(ctx context.Context, cmd any, modify func(*message.Message) error) error
}

// SendWithReply sends command to the command bus and receives a replies of the command handler.
// It returns a channel with replies, cancel function and error.
//
// SendWithReply can be cancelled by calling cancel function or by cancelling context or
// When SendWithReply is canceled, the returned channel is closed as well.
// by exceeding the timeout set in the backend (if set).
// Warning: It's important to cancel the function, because it's listening for the replies in the background.
// Lack of cancelling the function can lead to subscriber leak.
//
// SendWithReply can listen for handlers with results (NewCommandHandlerWithResult) and without results (NewCommandHandler).
// If you are listening for handlers without results, you should pass `NoResult` or `struct{}` as `Result` generic type:
//
//	 replyCh, cancel, err := requestreply.SendWithReply[requestreply.NoResult](
//			context.Background(),
//			ts.CommandBus,
//			ts.RequestReplyBackend,
//			&TestCommand{ID: "1"},
//		)
//
// If `NewCommandHandlerWithResult` handler returns a specific type, you should pass it as `Result` generic type:
//
//	 replyCh, cancel, err := requestreply.SendWithReply[SomeTypeReturnedByHandler](
//			context.Background(),
//			ts.CommandBus,
//			ts.RequestReplyBackend,
//			&TestCommand{ID: "1"},
//		)
//
// SendWithReply will send the replies to the channel until the context is cancelled or the timeout is exceeded.
func SendWithReply[Result any](
	ctx context.Context,
	c CommandBus,
	backend Backend[Result],
	cmd any,
) (replCh <-chan Reply[Result], cancel func(), err error) {
	ctx, cancel = context.WithCancel(ctx)

	defer func() {
		if err != nil {
			cancel()
		}
	}()

	operationID := watermill.NewUUID()

	replyChan, err := backend.ListenForNotifications(ctx, BackendListenForNotificationsParams{
		Command:     cmd,
		OperationID: OperationID(operationID),
	})
	if err != nil {
		return nil, cancel, errors.Wrap(err, "cannot listen for reply")
	}

	if err := c.SendWithModifiedMessage(ctx, cmd, func(m *message.Message) error {
		m.Metadata.Set(OperationIDMetadataKey, operationID)
		return nil
	}); err != nil {
		return nil, cancel, errors.Wrap(err, "cannot send command")
	}

	return replyChan, cancel, nil
}
