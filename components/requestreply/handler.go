package requestreply

import (
	"context"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/pkg/errors"
)

// NewCommandHandler creates a new CommandHandler which supports request-reply pattern.
// The result handler, is handler compatible with cqrs.CommandHandler.
//
// The logic if command should be acked or not is based on logic of the Backend.
// For example, for the PubSubBackend, it depends on `PubSubBackendConfig.AckCommandErrors` option.
func NewCommandHandler[Command any](
	handlerName string,
	backend Backend[struct{}],
	handleFunc func(ctx context.Context, cmd *Command) error,
) cqrs.CommandHandler {
	return cqrs.NewCommandHandler(handlerName, func(ctx context.Context, cmd *Command) error {
		handlerErr := handleFunc(ctx, cmd)

		originalMessage, err := originalCommandMsgFromCtx(ctx)
		if err != nil {
			return err
		}

		return backend.OnCommandProcessed(ctx, BackendOnCommandProcessedParams[struct{}]{
			Command:        cmd,
			CommandMessage: originalMessage,
			HandleErr:      handlerErr,
		})
	})
}

// NewCommandHandlerWithResult creates a new CommandHandler which supports request-reply pattern with result.
// The result handler, is handler compatible with cqrs.CommandHandler.
//
// In addition to cqrs.CommandHandler, it also allows to return result from handler.
// The result is passed to the Backend implementation and sent to the caller.
//
// The logic if command should be acked or not is based on logic of the Backend.
// For example, for the PubSubBackend, it depends on `PubSubBackendConfig.AckCommandErrors` option.
//
// The reply is sent to the caller, even if handler returns an error.
func NewCommandHandlerWithResult[Command any, Result any](
	handlerName string,
	backend Backend[Result],
	handleFunc func(ctx context.Context, cmd *Command) (Result, error),
) cqrs.CommandHandler {
	return cqrs.NewCommandHandler(handlerName, func(ctx context.Context, cmd *Command) error {
		resp, handlerErr := handleFunc(ctx, cmd)

		originalMessage, err := originalCommandMsgFromCtx(ctx)
		if err != nil {
			return err
		}

		return backend.OnCommandProcessed(ctx, BackendOnCommandProcessedParams[Result]{
			Command:        cmd,
			CommandMessage: originalMessage,
			HandlerResult:  resp,
			HandleErr:      handlerErr,
		})
	})
}

func originalCommandMsgFromCtx(ctx context.Context) (*message.Message, error) {
	originalMessage := cqrs.OriginalMessageFromCtx(ctx)
	if originalMessage == nil {
		// this should not happen, as long as cqrs.CommandProcessor is used - but it's not mandatory
		// in this case it's enough to use cqrs.CtxWithOriginalMessage
		return nil, errors.New(
			"original message not found in context, did you passed context correctly everywhere? " +
				"did you used cqrs.CommandProcessor? " +
				"if you are using custom implementation, please call cqrs.CtxWithOriginalMessage on context passed to handler",
		)
	}
	return originalMessage, nil
}
