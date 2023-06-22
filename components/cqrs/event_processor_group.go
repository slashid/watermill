package cqrs

import (
	stdErrors "errors"
	"fmt"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/pkg/errors"
)

type EventGroupProcessorConfig struct {
	// GenerateHandlerGroupSubscribeTopic is used to generate topic for subscribing to events for handler groups.
	// This option is required for EventProcessor if handler groups are used.
	GenerateHandlerGroupSubscribeTopic GenerateEventHandlerGroupSubscribeTopicFn

	// GroupSubscriberConstructor is used to create subscriber for GroupEventHandler.
	// This function is called for every events group once - thanks to that it's possible to have one subscription per group.
	// It's useful, when we are processing events from one stream and we want to do it in order.
	GroupSubscriberConstructor EventsGroupSubscriberConstructorWithParams

	// OnGroupHandle works like OnHandle, but is called for group handlers instead.
	// OnHandle is not called for handlers group.
	// This option is not required.
	OnGroupHandle OnGroupEventHandleFn

	// AckOnUnknownEvent is used to decide if message should be acked if event has no handler defined.
	AckOnUnknownEvent bool

	// Marshaler is used to marshal and unmarshal events.
	// It is required.
	Marshaler CommandEventMarshaler

	// Logger instance used to log.
	// If not provided, watermill.NopLogger is used.
	Logger watermill.LoggerAdapter
}

func (c *EventGroupProcessorConfig) setDefaults() {
	if c.Logger == nil {
		c.Logger = watermill.NopLogger{}
	}
}

func (c EventGroupProcessorConfig) Validate() error {
	var err error

	if c.Marshaler == nil {
		err = stdErrors.Join(err, errors.New("missing Marshaler"))
	}

	if c.GenerateHandlerGroupSubscribeTopic == nil {
		err = stdErrors.Join(err, errors.New("missing GenerateHandlerGroupTopic while GroupSubscriberConstructor is provided"))
	}
	if c.GroupSubscriberConstructor == nil {
		err = stdErrors.Join(err, errors.New("missing GroupSubscriberConstructor while GenerateHandlerGroupTopic is provided"))
	}

	return err
}

type GenerateEventHandlerGroupSubscribeTopicFn func(GenerateEventHandlerGroupTopicParams) (string, error)

type GenerateEventHandlerGroupTopicParams struct {
	EventGroupName     string
	EventGroupHandlers []GroupEventHandler
}

type EventsGroupSubscriberConstructorWithParams func(EventsGroupSubscriberConstructorParams) (message.Subscriber, error)

type EventsGroupSubscriberConstructorParams struct {
	EventGroupName     string
	EventGroupHandlers []GroupEventHandler
}

type OnGroupEventHandleFn func(params OnGroupEventHandleParams) error

type OnGroupEventHandleParams struct {
	GroupName string
	Handler   GroupEventHandler

	Event     any
	EventName string

	// Message is never nil and can be modified.
	Message *message.Message
}

// EventGroupProcessor determines which EventHandler should handle event received from event bus.
// todo!
type EventGroupProcessor struct {
	groupEventHandlers map[string][]GroupEventHandler

	config EventGroupProcessorConfig
}

// NewEventProcessorWithConfig creates a new EventProcessor.
// todo!
func NewEventGroupProcessorWithConfig(config EventGroupProcessorConfig) (*EventGroupProcessor, error) {
	config.setDefaults()

	if err := config.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid config EventProcessor")
	}

	return &EventGroupProcessor{
		groupEventHandlers: map[string][]GroupEventHandler{},
		config:             config,
	}, nil
}

// AddHandlersGroup adds a new list of GroupEventHandler to the EventProcessor.
//
// Compared to AddHandler, AddHandlersGroup allows to have multiple handlers that share the same subscriber instance.
//
// It's required to call AddHandlersToRouter to add the handlers to the router after calling AddHandlersGroup.
// Handlers group needs to be unique within the EventProcessor instance.
// todo: rename?
func (p *EventGroupProcessor) AddHandlersGroup(handlerName string, handlers ...GroupEventHandler) error {
	if len(handlers) == 0 {
		return errors.New("no handlers provided")
	}
	if _, ok := p.groupEventHandlers[handlerName]; ok {
		return fmt.Errorf("event handler group '%s' already exists", handlerName)
	}

	p.groupEventHandlers[handlerName] = handlers

	return nil
}

// AddHandlersToRouter adds the EventProcessor's handlers to the given router.
// It should be called only once per EventProcessor instance.
func (p EventGroupProcessor) AddHandlersToRouter(r *message.Router) error {
	if len(p.groupEventHandlers) == 0 {
		return errors.New("EventProcessor has no handlers, did you call AddHandler?")
	}

	for groupName := range p.groupEventHandlers {
		handlersGroup := p.groupEventHandlers[groupName]

		for i, handler := range handlersGroup {
			if err := validateEvent(handler.NewEvent()); err != nil {
				return fmt.Errorf(
					"invalid event for handler %T (num %d) in group %s: %w",
					handler,
					i,
					groupName,
					err,
				)
			}
		}

		if p.config.GenerateHandlerGroupSubscribeTopic == nil {
			return errors.New("missing GenerateHandlerGroupSubscribeTopic config option")
		}

		topicName, err := p.config.GenerateHandlerGroupSubscribeTopic(GenerateEventHandlerGroupTopicParams{
			EventGroupName:     groupName,
			EventGroupHandlers: handlersGroup,
		})
		if err != nil {
			return errors.Wrapf(err, "cannot generate topic name for handler group %s", groupName)
		}

		logger := p.config.Logger.With(watermill.LogFields{
			"event_handler_group_name": groupName,
			"topic":                    topicName,
		})

		handlerFunc, err := p.routerHandlerGroupFunc(handlersGroup, groupName, logger)
		if err != nil {
			return err
		}

		subscriber, err := p.config.GroupSubscriberConstructor(EventsGroupSubscriberConstructorParams{
			EventGroupName:     groupName,
			EventGroupHandlers: handlersGroup,
		})
		if err != nil {
			return errors.Wrap(err, "cannot create subscriber for event processor")
		}

		if err := addHandlerToRouter(p.config.Logger, r, groupName, topicName, handlerFunc, subscriber); err != nil {
			return err
		}
	}

	return nil
}

func (p EventGroupProcessor) routerHandlerGroupFunc(handlers []GroupEventHandler, groupName string, logger watermill.LoggerAdapter) (message.NoPublishHandlerFunc, error) {
	return func(msg *message.Message) error {
		messageEventName := p.config.Marshaler.NameFromMessage(msg)

		for _, handler := range handlers {
			initEvent := handler.NewEvent()
			expectedEventName := p.config.Marshaler.Name(initEvent)

			event := handler.NewEvent()

			if messageEventName != expectedEventName {
				logger.Trace("Received different event type than expected, ignoring", watermill.LogFields{
					"message_uuid":        msg.UUID,
					"expected_event_type": expectedEventName,
					"received_event_type": messageEventName,
				})
				continue
			}

			logger.Debug("Handling event", watermill.LogFields{
				"message_uuid":        msg.UUID,
				"received_event_type": messageEventName,
			})

			if err := p.config.Marshaler.Unmarshal(msg, event); err != nil {
				return err
			}

			handle := func(params OnGroupEventHandleParams) error {
				return params.Handler.Handle(params.Message.Context(), params.Event)
			}
			if p.config.OnGroupHandle != nil {
				handle = p.config.OnGroupHandle
			}

			err := handle(OnGroupEventHandleParams{
				GroupName: groupName,
				Handler:   handler,
				EventName: messageEventName,
				Event:     event,
				Message:   msg,
			})
			if err != nil {
				logger.Debug("Error when handling event", watermill.LogFields{"err": err})
				return err
			}

			return nil
		}

		if !p.config.AckOnUnknownEvent {
			return fmt.Errorf("no handler found for event %s", p.config.Marshaler.NameFromMessage(msg))
		} else {
			logger.Trace("Received event can't be handled by any handler in handler group", watermill.LogFields{
				"message_uuid":        msg.UUID,
				"received_event_type": messageEventName,
			})
			return nil
		}
	}, nil
}

type groupEventHandlerToEventHandlerAdapter struct {
	GroupEventHandler
	handlerName string
}

func (g groupEventHandlerToEventHandlerAdapter) HandlerName() string {
	return g.handlerName
}