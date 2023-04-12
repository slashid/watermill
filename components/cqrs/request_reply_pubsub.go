package cqrs

import (
	"context"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/pkg/errors"
)

type PubSubRequestReplyMarshaler interface {
	Marshal(v interface{}) (*message.Message, error)
	Unmarshal(msg *message.Message, v interface{}) (err error)
}

// PubSubRequestReply is a RequestReplyBackend that uses Pub/Sub to transport commands and replies.
type PubSubRequestReply struct {
	config PubSubRequestReplyConfig
}

// NewPubSubRequestReply creates a new PubSubRequestReply.
func NewPubSubRequestReply(config PubSubRequestReplyConfig) (*PubSubRequestReply, error) {
	config.setDefaults()

	if err := config.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid config")
	}

	return &PubSubRequestReply{
		config: config,
	}, nil
}

type PubSubRequestReplySubscriberConstructorFn func(PubSubRequestReplySubscriberConstructorParams) (message.Subscriber, error)

type PubSubRequestReplySubscriberConstructorParams struct {
	CommandUUID    string
	CommandMessage *message.Message
	Command        any
}

type PubSubRequestReplyTopicGeneratorFn func(PubSubRequestReplyTopicGeneratorParams) (string, error)

type PubSubRequestReplyTopicGeneratorParams struct {
	CommandUUID    string
	CommandMessage *message.Message
	Command        any
}

type PubSubRequestReplyConfig struct {
	Publisher                      message.Publisher
	SubscriberConstructor          PubSubRequestReplySubscriberConstructorFn
	GenerateReplyNotificationTopic PubSubRequestReplyTopicGeneratorFn

	Marshaler PubSubRequestReplyMarshaler

	Logger watermill.LoggerAdapter

	ListenForReplyTimeout *time.Duration

	ModifyNotificationMessage func(msg *message.Message, handleErr error) error
}

func (p *PubSubRequestReplyConfig) setDefaults() {
	if p.Logger == nil {
		p.Logger = watermill.NopLogger{}
	}

	if p.Marshaler == nil {
		p.Marshaler = JSONMarshaler{}
	}
}

func (p *PubSubRequestReplyConfig) Validate() error {
	if p.Publisher == nil {
		return errors.New("publisher cannot be nil")
	}
	if p.SubscriberConstructor == nil {
		return errors.New("subscriber constructor cannot be nil")
	}
	if p.GenerateReplyNotificationTopic == nil {
		return errors.New("GenerateReplyNotificationTopic cannot be nil")
	}

	return nil
}

const notifyWhenExecutedMetadataKey = "_watermill_notify_when_executed"

func (p PubSubRequestReply) ModifyCommandMessageBeforePublish(cmdMsg *message.Message, command any) error {
	cmdMsg.Metadata.Set(notifyWhenExecutedMetadataKey, "1")

	return nil
}

func (p PubSubRequestReply) ListenForReply(
	ctx context.Context,
	cmdMsg *message.Message,
	command any,
) (<-chan CommandReply, error) {
	start := time.Now()

	// this needs to be done before publishing the message to avoid race condition
	notificationsSubscriber, err := p.config.SubscriberConstructor(
		PubSubRequestReplySubscriberConstructorParams{
			CommandUUID:    cmdMsg.UUID,
			CommandMessage: cmdMsg,
			Command:        command,
		},
	)
	if err != nil {
		return nil, errors.Wrap(err, "cannot create request/reply notifications subscriber")
	}

	replyNotificationTopic, err := p.config.GenerateReplyNotificationTopic(
		PubSubRequestReplyTopicGeneratorParams{
			CommandUUID:    cmdMsg.UUID,
			CommandMessage: cmdMsg,
			Command:        command,
		},
	)
	if err != nil {
		return nil, errors.Wrap(err, "cannot generate request/reply notifications topic")
	}

	var cancel context.CancelFunc
	if p.config.ListenForReplyTimeout != nil {
		ctx, cancel = context.WithTimeout(ctx, *p.config.ListenForReplyTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	notifyMsgs, err := notificationsSubscriber.Subscribe(ctx, replyNotificationTopic)
	if err != nil {
		cancel()
		return nil, errors.Wrap(err, "cannot subscribe to request/reply notifications topic")
	}

	p.config.Logger.Debug("Subscribed to request/reply notifications topic", nil)

	replyChan := make(chan CommandReply, 1)

	go func() {
		defer close(replyChan)
		defer cancel()

		for {
			select {
			case <-ctx.Done():
				replyChan <- CommandReply{
					Err: ReplyTimeoutError{time.Since(start), ctx.Err()},
				}
				return
			case notifyMsg, ok := <-notifyMsgs:
				if !ok {
					// subscriber is closed
					replyChan <- CommandReply{
						Err: ReplyTimeoutError{time.Since(start), fmt.Errorf("subscriber closed")},
					}
					return
				}

				if ok, handlerErr := p.handleNotifyMsg(notifyMsg, cmdMsg.UUID); ok {
					reply := CommandReply{
						ReplyMsg: notifyMsg,
					}
					if handlerErr != nil {
						reply.Err = CommandHandlerError{handlerErr}
					}

					replyChan <- reply
					continue
				}
			}
		}
	}()

	return replyChan, nil
}

const handledCommandUuidMetadataKey = "_watermill_command_message_uuid"

func (p PubSubRequestReply) OnCommandProcessed(cmdMsg *message.Message, cmd any, handleErr error) error {
	if cmdMsg.Metadata.Get(notifyWhenExecutedMetadataKey) != "1" {
		p.config.Logger.Debug(fmt.Sprintf("RequestReply is enabled, but %s is missing", notifyWhenExecutedMetadataKey), nil)
		return nil
	}

	p.config.Logger.Debug("Sending request reply", nil)

	// we are using protobuf message, so it will work both with proto and json marshaler
	notification := &RequestReplyNotification{}
	if handleErr != nil {
		notification.Error = handleErr.Error()
		notification.HasError = true
	}

	notificationMsg, err := p.config.Marshaler.Marshal(notification)
	if err != nil {
		return errors.Wrap(err, "cannot marshal request reply notification")
	}

	notificationMsg.SetContext(cmdMsg.Context())
	notificationMsg.Metadata.Set(handledCommandUuidMetadataKey, cmdMsg.UUID)

	if p.config.ModifyNotificationMessage != nil {
		if err := p.config.ModifyNotificationMessage(notificationMsg, handleErr); err != nil {
			return errors.Wrap(err, "cannot modify notification message")
		}
	}

	replyTopic, err := p.config.GenerateReplyNotificationTopic(PubSubRequestReplyTopicGeneratorParams{
		CommandUUID:    cmdMsg.UUID,
		CommandMessage: cmdMsg,
		Command:        cmd,
	})
	if err != nil {
		return errors.Wrap(err, "cannot generate request/reply notify topic")
	}

	if err := p.config.Publisher.Publish(replyTopic, notificationMsg); err != nil {
		return errors.Wrap(err, "cannot publish command executed message")
	}

	return nil
}

func (p PubSubRequestReply) handleNotifyMsg(msg *message.Message, expectedCommandUuid string) (bool, error) {
	defer msg.Ack()

	if msg.Metadata.Get(handledCommandUuidMetadataKey) != expectedCommandUuid {
		// todo: lower log level
		p.config.Logger.Debug("Received notify message with different command UUID", nil)
		return false, nil
	}

	// we are using protobuf message, so it will work both with proto and json marshaler
	notification := &RequestReplyNotification{}
	if err := p.config.Marshaler.Unmarshal(msg, notification); err != nil {
		return false, errors.Wrap(err, "cannot unmarshal request reply notification")
	}

	if notification.HasError {
		return true, errors.New(notification.Error)
	} else {
		return true, nil
	}
}
