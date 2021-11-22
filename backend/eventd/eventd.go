package eventd

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"

	corev2 "github.com/sensu/sensu-go/api/core/v2"
	corev3 "github.com/sensu/sensu-go/api/core/v3"
	"github.com/sensu/sensu-go/backend/keepalived"
	"github.com/sensu/sensu-go/backend/liveness"
	"github.com/sensu/sensu-go/backend/messaging"
	"github.com/sensu/sensu-go/backend/store"
	"github.com/sensu/sensu-go/backend/store/cache"
	storev2 "github.com/sensu/sensu-go/backend/store/v2"
	metricspkg "github.com/sensu/sensu-go/metrics"
	utillogging "github.com/sensu/sensu-go/util/logging"
)

const (
	// ComponentName identifies Eventd as the component/daemon implemented in this
	// package.
	ComponentName = "eventd"

	// EventsProcessedCounterVec is the name of the prometheus counter vec used to count events processed.
	EventsProcessedCounterVec = "sensu_go_events_processed"

	// EventsProcessedLabelName is the name of the label which describes if an
	// event was processed successfully or not.
	EventsProcessedLabelName = "status"

	// EventsProcessedLabelSuccess is the value to use for the status label if
	// an event has been processed successfully.
	EventsProcessedLabelSuccess = "success"

	// EventsProcessedLabelError is the value to use for the status label if
	// an event has errored during processing.
	EventsProcessedLabelError = "error"

	// EventsProcessedTypeLabelName is the name of the label which describes
	// what type of event is being processed.
	EventsProcessedTypeLabelName = "type"

	// EventsProcessedTypeLabelUnknown is the value to use for the type label if
	// the event type is not known.
	EventsProcessedTypeLabelUnknown = "unknown"

	// EventsProcessedTypeLabelCheck is the value to use for the type label if
	// the event has a check.
	EventsProcessedTypeLabelCheck = "check"

	// EventProcessedTypeLabelMetrics is the value to use for the type label if
	// the event doesn't have a check (metrics-only).
	EventsProcessedTypeLabelMetrics = "metrics"

	// EventHandlerDuration is the name of the prometheus summary vec used to
	// track average latencies of event handling.
	EventHandlerDuration = "sensu_go_event_handler_duration"

	// EventHandlersBusyGaugeVec is the name of the prometheus gauge vec used to
	// track how many eventd handlers are busy processing events.
	EventHandlersBusyGaugeVec = "sensu_go_event_handlers_busy"

	// CreateProxyEntityDuration is the name of the prometheus summary vec used
	// to track average latencies of proxy entity creation.
	CreateProxyEntityDuration = "sensu_go_eventd_create_proxy_entity_duration"

	// UpdateEventDuration is the name of the prometheus summary vec used to
	// track average latencies of updating events.
	UpdateEventDuration = "sensu_go_eventd_update_event_duration"

	// BusPublishDuration is the name of the prometheus summary vec used to
	// track average latencies of publishing to the bus.
	BusPublishDuration = "sensu_go_eventd_bus_publish_duration"

	// LivenessFactoryDuration is the name of the prometheus summary vec used to
	// track average latencies of calls to the liveness factory.
	LivenessFactoryDuration = "sensu_go_eventd_liveness_factory_duration"

	// SwitchesAliveDuration is the name of the prometheus summary vec used to
	// track average latencies of calls to switches.Alive.
	SwitchesAliveDuration = "sensu_go_eventd_switches_alive_duration"

	// SwitchesBuryDuration is the name of the prometheus summary vec used to
	// track average latencies of calls to switches.Bury.
	SwitchesBuryDuration = "sensu_go_eventd_switches_bury_duration"
)

var (
	logger = logrus.WithFields(logrus.Fields{
		"component": ComponentName,
	})

	// EventsProcessed counts the number of sensu go events processed.
	EventsProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: EventsProcessedCounterVec,
			Help: "The total number of processed events",
		},
		[]string{EventsProcessedLabelName, EventsProcessedTypeLabelName},
	)

	eventHandlerDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       EventHandlerDuration,
			Help:       "event handler latency distribution",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{metricspkg.StatusLabelName, metricspkg.EventTypeLabelName},
	)

	eventHandlersBusy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: EventHandlersBusyGaugeVec,
			Help: "The number of event handlers currently processing",
		},
		[]string{},
	)

	createProxyEntityDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       CreateProxyEntityDuration,
			Help:       "proxy entity creation latency distribution in eventd",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{metricspkg.StatusLabelName},
	)

	updateEventDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       UpdateEventDuration,
			Help:       "event updating latency distribution in eventd",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{metricspkg.StatusLabelName},
	)

	busPublishDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       BusPublishDuration,
			Help:       "bus publishing latency distribution in eventd",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{metricspkg.StatusLabelName, metricspkg.EventTypeLabelName},
	)

	livenessFactoryDuration = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Name:       LivenessFactoryDuration,
			Help:       "liveness factory latency distribution in eventd",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
	)

	switchesAliveDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       SwitchesAliveDuration,
			Help:       "switches.Alive() latency distribution in eventd",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{metricspkg.StatusLabelName},
	)

	switchesBuryDuration = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       SwitchesBuryDuration,
			Help:       "switches.Bury() latency distribution in eventd",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
		[]string{metricspkg.StatusLabelName},
	)
)

const deletedEventSentinel = -1

// Eventd handles incoming sensu events and stores them in etcd.
type Eventd struct {
	store               storev2.Interface
	eventStore          store.EventStore
	client              *clientv3.Client
	bus                 messaging.MessageBus
	workerCount         int
	livenessFactory     liveness.Factory
	eventChan           chan interface{}
	keepaliveChan       chan interface{}
	subscription        messaging.Subscription
	errChan             chan error
	mu                  *sync.Mutex
	shutdownChan        chan struct{}
	wg                  *sync.WaitGroup
	Logger              Logger
	silencedCache       Cache
	logPath             string
	logBufferSize       int
	logBufferWait       time.Duration
	logParallelEncoders bool
}

// Cache interfaces the cache.Resource struct for easier testing
type Cache interface {
	Get(namespace string) []cache.Value
}

// Option is a functional option.
type Option func(*Eventd) error

// Config configures Eventd
type Config struct {
	Store               storev2.Interface
	EventStore          store.EventStore
	Bus                 messaging.MessageBus
	LivenessFactory     liveness.Factory
	Client              *clientv3.Client
	BufferSize          int
	WorkerCount         int
	LogPath             string
	LogBufferSize       int
	LogBufferWait       time.Duration
	LogParallelEncoders bool
}

// New creates a new Eventd.
func New(ctx context.Context, c Config, opts ...Option) (*Eventd, error) {
	if c.BufferSize == 0 {
		logger.Warn("BufferSize not configured")
		c.BufferSize = 1
	}
	if c.WorkerCount == 0 {
		logger.Warn("WorkerCount not configured")
		c.WorkerCount = 1
	}

	e := &Eventd{
		store:               c.Store,
		eventStore:          c.EventStore,
		bus:                 c.Bus,
		workerCount:         c.WorkerCount,
		livenessFactory:     c.LivenessFactory,
		errChan:             make(chan error, 1),
		shutdownChan:        make(chan struct{}, 1),
		eventChan:           make(chan interface{}, c.BufferSize),
		keepaliveChan:       make(chan interface{}, c.BufferSize),
		wg:                  &sync.WaitGroup{},
		mu:                  &sync.Mutex{},
		logPath:             c.LogPath,
		logBufferSize:       c.LogBufferSize,
		logBufferWait:       c.LogBufferWait,
		logParallelEncoders: c.LogParallelEncoders,
		Logger:              NoopLogger{},
		client:              c.Client,
	}

	for _, o := range opts {
		if err := o(e); err != nil {
			return nil, err
		}
	}

	// Initialize labels & register metric families with Prometheus
	EventsProcessed.WithLabelValues(EventsProcessedLabelSuccess, EventsProcessedTypeLabelCheck)
	EventsProcessed.WithLabelValues(EventsProcessedLabelSuccess, EventsProcessedTypeLabelMetrics)
	EventsProcessed.WithLabelValues(EventsProcessedLabelError, EventsProcessedTypeLabelUnknown)
	EventsProcessed.WithLabelValues(EventsProcessedLabelError, EventsProcessedTypeLabelCheck)

	eventHandlerDuration.WithLabelValues(metricspkg.StatusLabelSuccess, metricspkg.EventTypeLabelCheck)
	eventHandlerDuration.WithLabelValues(metricspkg.StatusLabelSuccess, metricspkg.EventTypeLabelMetrics)
	eventHandlerDuration.WithLabelValues(metricspkg.StatusLabelSuccess, metricspkg.EventTypeLabelUnknown)
	eventHandlerDuration.WithLabelValues(metricspkg.StatusLabelError, metricspkg.EventTypeLabelCheck)
	eventHandlerDuration.WithLabelValues(metricspkg.StatusLabelError, metricspkg.EventTypeLabelMetrics)
	eventHandlerDuration.WithLabelValues(metricspkg.StatusLabelError, metricspkg.EventTypeLabelUnknown)

	createProxyEntityDuration.WithLabelValues(metricspkg.StatusLabelSuccess)
	createProxyEntityDuration.WithLabelValues(metricspkg.StatusLabelError)

	updateEventDuration.WithLabelValues(metricspkg.StatusLabelSuccess)
	updateEventDuration.WithLabelValues(metricspkg.StatusLabelError)

	busPublishDuration.WithLabelValues(metricspkg.StatusLabelSuccess, metricspkg.EventTypeLabelCheck)
	busPublishDuration.WithLabelValues(metricspkg.StatusLabelSuccess, metricspkg.EventTypeLabelMetrics)
	busPublishDuration.WithLabelValues(metricspkg.StatusLabelError, metricspkg.EventTypeLabelCheck)
	busPublishDuration.WithLabelValues(metricspkg.StatusLabelError, metricspkg.EventTypeLabelMetrics)

	_ = prometheus.Register(EventsProcessed)
	_ = prometheus.Register(eventHandlerDuration)
	_ = prometheus.Register(eventHandlersBusy)
	_ = prometheus.Register(createProxyEntityDuration)
	_ = prometheus.Register(updateEventDuration)
	_ = prometheus.Register(busPublishDuration)
	_ = prometheus.Register(livenessFactoryDuration)
	_ = prometheus.Register(switchesAliveDuration)
	_ = prometheus.Register(switchesBuryDuration)

	return e, nil
}

// Receiver returns the event receiver channel.
func (e *Eventd) Receiver() chan<- interface{} {
	return e.eventChan
}

// Start eventd.
func (e *Eventd) Start(ctx context.Context) error {
	if e.client != nil {
		// TODO(eric): this is technical debt; the tests assume that eventd can
		// work without a cache or etcd client.
		cache, err := cache.New(ctx, e.client, &corev2.Silenced{}, false)
		if err != nil {
			return err
		}
		e.silencedCache = cache
	}

	e.wg.Add(e.workerCount)
	sub, err := e.bus.Subscribe(messaging.TopicEventRaw, "eventd", e)
	e.subscription = sub
	if err != nil {
		return err
	}

	// Start the event logger if configured
	if e.logPath != "" {
		logger := FileLogger{
			Path:                 e.logPath,
			BufferSize:           e.logBufferSize,
			BufferWait:           e.logBufferWait,
			Bus:                  e.bus,
			ParallelJSONEncoding: e.logParallelEncoders,
		}
		logger.Start()

		e.Logger = &logger
	}

	e.startHandlers(ctx)

	return nil
}

func withEventFields(e interface{}, logger *logrus.Entry) *logrus.Entry {
	event, _ := e.(*corev2.Event)
	if event != nil {
		fields := utillogging.EventFields(event, false)
		logger = logger.WithFields(fields)
	}
	return logger
}

func (e *Eventd) startHandlers(ctx context.Context) {
	for i := 0; i < e.workerCount; i++ {
		go func() {
			defer e.wg.Done()

			for {
				select {
				case <-ctx.Done():
					return

				case msg, ok := <-e.eventChan:
					eventHandlersBusy.WithLabelValues().Inc()

					// The message bus will close channels when it's shut down which means
					// we will end up reading from a closed channel. If it's closed,
					// return from this goroutine and emit a fatal error. It is then
					// the responsility of eventd's parent to shutdown eventd.
					if !ok {
						select {
						// If this channel send doesn't occur immediately it means
						// another goroutine has placed an error there already; we
						// don't need to send another.
						case e.errChan <- errors.New("event channel closed"):
						default:
						}
						return
					}
					for {
						select {
						case keepMsg, ok := <-e.keepaliveChan:
							if !ok {
								goto DRAINED
							}
							if _, err := e.handleMessage(ctx, keepMsg); err != nil {
								logger := withEventFields(msg, logger)
								logger.WithError(err).Error("error handling event")
							}
						case <-ctx.Done():
							return
						default:
							goto DRAINED
						}
					}
				DRAINED:
					if _, err := e.handleMessage(ctx, msg); err != nil {
						logger := withEventFields(msg, logger)
						logger.WithError(err).Error("error handling event")
					}
					eventHandlersBusy.WithLabelValues().Dec()
				case msg, ok := <-e.keepaliveChan:
					eventHandlersBusy.WithLabelValues().Inc()
					if !ok {
						select {
						// If this channel send doesn't occur immediately it means
						// another goroutine has placed an error there already; we
						// don't need to send another.
						case e.errChan <- errors.New("event channel closed"):
						default:
						}
						return
					}
					if _, err := e.handleMessage(ctx, msg); err != nil {
						logger := withEventFields(msg, logger)
						logger.WithError(err).Error("error handling event")
					}
					eventHandlersBusy.WithLabelValues().Dec()
				}
			}
		}()
	}
}

// eventKey creates a key to identify the event for liveness monitoring
func eventKey(event *corev2.Event) string {
	// Typically we want the entity name to be the thing we monitor, but if
	// it's a round robin check, and there is no proxy entity, then use
	// the check name instead.
	if event.Check.RoundRobin && event.Entity.EntityClass != corev2.EntityProxyClass {
		return path.Join(event.Check.Namespace, event.Check.Name)
	}
	return path.Join(event.Entity.Namespace, event.Check.Name, event.Entity.Name)
}

func (e *Eventd) publishEventWithDuration(event *corev2.Event) (fErr error) {
	begin := time.Now()
	defer func() {
		duration := time.Since(begin)
		status := metricspkg.StatusLabelSuccess
		if fErr != nil {
			status = metricspkg.StatusLabelError
		}
		eventType := metricspkg.EventTypeLabelMetrics
		if event.HasCheck() {
			eventType = metricspkg.EventTypeLabelCheck
		}
		busPublishDuration.
			WithLabelValues(status, eventType).
			Observe(float64(duration) / float64(time.Millisecond))
	}()

	return e.bus.Publish(messaging.TopicEvent, event)
}

func (e *Eventd) updateEventWithDuration(ctx context.Context, event *corev2.Event) (fEvent, fPrevEvent *corev2.Event, fErr error) {
	begin := time.Now()
	defer func() {
		duration := time.Since(begin)
		status := metricspkg.StatusLabelSuccess
		if fErr != nil {
			status = metricspkg.StatusLabelError
		}
		updateEventDuration.
			WithLabelValues(status).
			Observe(float64(duration) / float64(time.Millisecond))
	}()

	return e.eventStore.UpdateEvent(ctx, event)
}

func (e *Eventd) handleMessage(ctx context.Context, msg interface{}) (fEvent *corev2.Event, fErr error) {
	then := time.Now()
	defer func() {
		duration := time.Since(then)

		// record the status of the handled event
		status := metricspkg.StatusLabelSuccess
		if fErr != nil {
			status = metricspkg.StatusLabelError
		}

		// record the event type of the handled event
		eventType := metricspkg.EventTypeLabelUnknown
		if fEvent != nil {
			if !fEvent.HasCheck() && fEvent.HasMetrics() {
				eventType = metricspkg.EventTypeLabelMetrics
			}
			if fEvent.HasCheck() {
				eventType = metricspkg.EventTypeLabelCheck
			}
		}

		eventHandlerDuration.
			WithLabelValues(status, eventType).
			Observe(float64(duration) / float64(time.Millisecond))
	}()
	event, ok := msg.(*corev2.Event)
	if !ok {
		EventsProcessed.WithLabelValues(EventsProcessedLabelError, EventsProcessedTypeLabelUnknown).Inc()
		return event, fmt.Errorf("received non-Event on event channel: %v", msg)
	}

	fields := utillogging.EventFields(event, false)
	logger.WithFields(fields).Info("eventd received event")

	// Validate the received event
	if err := event.Validate(); err != nil {
		EventsProcessed.WithLabelValues(EventsProcessedLabelError, EventsProcessedTypeLabelUnknown).Inc()
		return event, err
	}

	// If the event does not contain a check (rather, it contains metrics)
	// publish the event without writing to the store
	if !event.HasCheck() {
		e.Logger.Println(event)
		EventsProcessed.WithLabelValues(EventsProcessedLabelSuccess, EventsProcessedTypeLabelMetrics).Inc()
		return event, e.publishEventWithDuration(event)
	}

	ctx = context.WithValue(ctx, corev2.NamespaceKey, event.Entity.Namespace)

	// Create a proxy entity if required and update the event's entity with it,
	// but only if the event's entity is not an agent.
	if err := createProxyEntity(event, e.store); err != nil {
		EventsProcessed.WithLabelValues(EventsProcessedLabelError, EventsProcessedTypeLabelCheck).Inc()
		return event, err
	}

	// Add any silenced subscriptions to the event
	getSilenced(ctx, event, e.silencedCache)
	if len(event.Check.Silenced) > 0 {
		event.Check.IsSilenced = true
	}

	// Merge the new event with the stored event if a match is found
	event, prevEvent, err := e.updateEventWithDuration(ctx, event)
	if err != nil {
		EventsProcessed.WithLabelValues(EventsProcessedLabelError, EventsProcessedTypeLabelCheck).Inc()
		return event, err
	}

	e.Logger.Println(event)

	livenessFactoryTimer := prometheus.NewTimer(livenessFactoryDuration)
	switches := e.livenessFactory("eventd", e.dead, e.alive, logger)
	livenessFactoryTimer.ObserveDuration()
	switchKey := eventKey(event)

	if event.Check.Name == corev2.KeepaliveCheckName {
		goto NOTTL
	}

	if event.Check.Ttl > 0 {
		// Reset the switch
		timeout := int64(event.Check.Ttl)
		var err error
		aliveTimer := prometheus.NewTimer(prometheus.ObserverFunc(func(v float64) {
			status := metricspkg.StatusLabelSuccess
			if err != nil {
				status = metricspkg.StatusLabelError
			}
			switchesAliveDuration.WithLabelValues(status).Observe(v * float64(1000))
		}))
		err = switches.Alive(ctx, switchKey, timeout)
		aliveTimer.ObserveDuration()
		if err != nil {
			EventsProcessed.WithLabelValues(EventsProcessedLabelError, EventsProcessedTypeLabelCheck).Inc()
			return event, err
		}
	} else if (prevEvent != nil && prevEvent.Check.Ttl > 0) || event.Check.Ttl == deletedEventSentinel {
		// The check TTL has been disabled, there is no longer a need to track it
		logger.Debug("check ttl disabled")
		var err error
		buryTimer := prometheus.NewTimer(prometheus.ObserverFunc(func(v float64) {
			status := metricspkg.StatusLabelSuccess
			if err != nil {
				status = metricspkg.StatusLabelError
			}
			switchesBuryDuration.WithLabelValues(status).Observe(v * float64(1000))
		}))
		err = switches.Bury(ctx, switchKey)
		buryTimer.ObserveDuration()
		if err != nil {
			// It's better to publish the event even if this fails, so
			// don't return the error here.
			logger.WithError(err).Error("error burying switch")
		}
	}

NOTTL:

	EventsProcessed.WithLabelValues(EventsProcessedLabelSuccess, EventsProcessedTypeLabelCheck).Inc()

	return event, e.publishEventWithDuration(event)
}

func (e *Eventd) alive(ctx context.Context, key string, prev liveness.State, leader bool) (bury bool) {
	lager := logger.WithFields(logrus.Fields{
		"status":          liveness.Alive.String(),
		"previous_status": prev.String()})

	namespace, check, entity, err := parseKey(key)
	if err != nil {
		lager.Error(err)
		return false
	}

	lager = lager.WithFields(logrus.Fields{
		"check":     check,
		"entity":    entity,
		"namespace": namespace})

	lager.Info("check TTL reset")

	return false
}

func (e *Eventd) dead(ctx context.Context, key string, prev liveness.State, leader bool) (bury bool) {
	if ctx.Err() != nil {
		return false
	}
	lager := logger.WithFields(logrus.Fields{
		"status":          liveness.Dead.String(),
		"previous_status": prev.String()})

	namespace, check, entity, err := parseKey(key)
	if err != nil {
		lager.Error(err)
		return false
	}

	lager = lager.WithFields(logrus.Fields{
		"check":     check,
		"entity":    entity,
		"namespace": namespace})

	lager.Warn("check TTL expired")

	// NOTE: To support check TTL for round robin scheduling, load all events
	// here, filter by check, and update all events involved in the round robin
	if entity == "" {
		lager.Error("round robin check ttl not supported")
		return true
	}

	ctx = store.NamespaceContext(ctx, namespace)

	// The entity has been deleted, and so there is no reason to track check
	// TTL for it anymore.
	config := corev3.NewEntityConfig(namespace, entity)
	req := storev2.NewResourceRequestFromResource(ctx, config)

	_, err = e.store.Get(req)
	if _, ok := err.(*store.ErrNotFound); ok {
		return true
	} else if err != nil {
		lager.WithError(err).Error("check ttl: error retrieving entity")
		if _, ok := err.(*store.ErrInternal); ok {
			// Fatal error
			select {
			case e.errChan <- err:
			case <-ctx.Done():
			}
		}
		return false
	}

	keepalive, err := e.eventStore.GetEventByEntityCheck(ctx, entity, "keepalive")
	if err != nil {
		lager.WithError(err).Error("check ttl: error retrieving keepalive event")
		return false
	}

	if keepalive != nil && keepalive.Check.Status > 0 {
		// The keepalive is failing. We don't want to also alert for check TTL,
		// or keep track of check TTL until the entity returns to life.
		return true
	}

	event, err := e.eventStore.GetEventByEntityCheck(ctx, entity, check)
	if err != nil {
		lager.WithError(err).Error("check ttl: error retrieving event")
		if _, ok := err.(*store.ErrInternal); ok {
			// Fatal error
			select {
			case e.errChan <- err:
			case <-ctx.Done():
			}
		}
		return false
	}

	if event == nil {
		// The user deleted the check event but not the entity
		return true
	}

	if leader {
		if err := e.handleFailure(ctx, event); err != nil {
			lager.WithError(err).Error("can't handle check TTL failure")
		}
	}

	return false
}

func parseKey(key string) (namespace, check, entity string, err error) {
	parts := strings.Split(key, "/")
	if len(parts) == 2 {
		return parts[0], parts[1], "", nil
	}
	if len(parts) == 3 {
		return parts[0], parts[1], parts[2], nil
	}
	return "", "", "", errors.New("bad key")
}

// handleFailure creates a check event with a warn status and publishes it to
// TopicEvent.
func (e *Eventd) handleFailure(ctx context.Context, event *corev2.Event) error {
	// don't update the event with ttl output for keepalives,
	// there is a different mechanism for that
	if event.Check.Name == keepalived.KeepaliveCheckName {
		return nil
	}

	entity := event.Entity
	ctx = context.WithValue(ctx, corev2.NamespaceKey, entity.Namespace)

	failedCheckEvent, err := e.createFailedCheckEvent(ctx, event)
	if err != nil {
		return err
	}
	updatedEvent, _, err := e.eventStore.UpdateEvent(ctx, failedCheckEvent)
	if err != nil {
		if _, ok := err.(*store.ErrInternal); ok {
			// Fatal error
			select {
			case e.errChan <- err:
			case <-ctx.Done():
			}
		}
		return err
	}

	e.Logger.Println(updatedEvent)
	return e.bus.Publish(messaging.TopicEvent, updatedEvent)
}

func (e *Eventd) createFailedCheckEvent(ctx context.Context, event *corev2.Event) (*corev2.Event, error) {
	if !event.HasCheck() {
		return nil, errors.New("event does not contain a check")
	}

	event, err := e.eventStore.GetEventByEntityCheck(
		ctx, event.Entity.Name, event.Check.Name,
	)
	if err != nil {
		if _, ok := err.(*store.ErrInternal); ok {
			// Fatal error
			select {
			case e.errChan <- err:
			case <-ctx.Done():
			}
		}
		return nil, err
	}

	check := corev2.NewCheck(corev2.NewCheckConfigFromFace(event.Check))
	output := fmt.Sprintf("Last check execution was %d seconds ago", time.Now().Unix()-event.Check.Executed)

	check.Output = output
	check.Status = 1
	check.State = corev2.EventFailingState
	check.Executed = time.Now().Unix()

	check.MergeWith(event.Check)

	event.Timestamp = time.Now().Unix()
	event.Check = check

	return event, nil
}

// Stop eventd.
func (e *Eventd) Stop() error {
	logger.Info("shutting down eventd")
	if err := e.subscription.Cancel(); err != nil {
		logger.WithError(err).Error("unable to unsubscribe from message bus")
	}
	defer close(e.eventChan)
	close(e.shutdownChan)
	e.wg.Wait()
	if e.Logger != nil {
		e.Logger.Stop()
	}
	return nil
}

// Err returns a channel to listen for terminal errors on.
func (e *Eventd) Err() <-chan error {
	return e.errChan
}

// Name returns the daemon name
func (e *Eventd) Name() string {
	return "eventd"
}

// Workers returns the number of configured worker goroutines.
func (e *Eventd) Workers() int {
	return e.workerCount
}
