package schedule

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/modernice/goes/event"
	"github.com/modernice/goes/event/eventstore"
	"github.com/modernice/goes/event/query"
	"github.com/modernice/goes/helper/streams"
	"github.com/modernice/goes/projection"
)

// The maximum debounce duration for which DefaultDebounceCap is used as the
// wait cap. For higher debounce durations, the wait cap is computed by doubling
// the duration. If the DebounceCap() option is provided to a schedule, this
// value is ignored.
const defaultDebounceBarrier = 2500 * time.Millisecond

// DefaultDebounceCap is the default debounce cap if the DebounceCap() option is
// not provided and the provided debounce duration is <= 2.5s.
//
// If the provided debounce duration is > 2.5s, the cap is set to the double of
// the duration. For example, a debounce duration of 3s will have a cap of 6s if
// the DebounceCap() option is not provided.
var DefaultDebounceCap = 5 * time.Second

// Continuous is a projection Schedule that creates projection Jobs on every
// specified published event:
//
//	var bus event.Bus
//	var store event.Store
//	var proj projection.Projection
//	s := schedule.Continuously(bus, store, []string{"foo", "bar", "baz"})
//	errs, err := s.Subscribe(context.TODO(), func(job projection.Job) error {
//		return job.Apply(job, proj)
//	})
type Continuous struct {
	*schedule

	bus                    event.Bus
	debounce               time.Duration
	debounceCap            time.Duration
	debounceCapManuallySet bool
}

// ContinuousOption is an option for the Continuous schedule.
type ContinuousOption func(*Continuous)

// Debounce returns a ContinuousOption that debounces projection Jobs by the
// given Duration. When multiple events are published within the given Duration,
// only 1 projection Job for all events will be created instead of 1 Job per
// Event.
//
//	var bus event.Bus
//	var store event.Store
//	var proj projection.Projection
//	s := schedule.Continuously(bus, store, []string{"foo", "bar", "baz"}, schedule.Debounce(time.Second))
//	errs, err := s.Subscribe(context.TODO(), func(job projection.Job) error {
//		return job.Apply(job, proj)
//	})
//
//	err := bus.Publish(
//		context.TODO(),
//		event.New("foo", ...),
//		event.New("bar", ...),
//		event.New("baz", ...),
//	)
func Debounce(d time.Duration) ContinuousOption {
	return func(c *Continuous) {
		c.debounce = d
	}
}

// DebounceCap returns a ContinuousOption that specifies the maximum wait time
// (cap) before force-triggering a projection job that was deferred by the
// Debounce() option.
//
// By default, the maximum wait time is determined by this heuristic: If the
// duration provided to the Debounce() option is <= 2.5s, the wait cap is set to
// DefaultDebounceCap, which is 5s. Otherwise the cap is computed by doubling
// the duration provided to Debounce(). For example, a debounce duration of 3s
// will have a cap of 6s.
func DebounceCap(cap time.Duration) ContinuousOption {
	return func(c *Continuous) {
		c.debounceCap = cap
		c.debounceCapManuallySet = true
	}
}

// Continuously returns a Continuous schedule that, when subscribed to,
// subscribes to events with the given eventNames to create projection Jobs
// for those events.
//
// # Debounce events
//
// It may be desirable to debounce the creation of projection Jobs to avoid
// creating a Job on every event if Events are published within a short
// interval:
//
//	var bus event.Bus
//	var store event.Store
//	s := schedule.Continuously(bus, store, []string{"foo", "bar", "baz"}, schedule.Debounce(time.Second))
func Continuously(bus event.Bus, store event.Store, eventNames []string, opts ...ContinuousOption) *Continuous {
	c := Continuous{
		schedule:    newSchedule(store, eventNames),
		bus:         bus,
		debounceCap: DefaultDebounceCap,
	}
	for _, opt := range opts {
		opt(&c)
	}

	return &c
}

// Subscribe subscribes to the schedule and returns a channel of asynchronous
// projection errors, or a single error if subscribing failed. When ctx is
// canceled, the subscription is canceled and the returned error channel closed.
//
// When a projection Job is created, the apply function is called with that Job.
// Use Job.Apply to apply the Job's events to a given projection:
//
//	var proj projection.Projection
//	var s *schedule.Continuous
//	s.Subscribe(context.TODO(), func(job projection.Job) error {
//		return job.Apply(job, proj)
//	})
//
// A Job provides helper functions to extract data from the Job's events. Query
// results are cached within a Job, so it is safe to call helper functions
// multiple times; the Job will figure out if it needs to actually perform the
// query or if it can return the cached result.
//
//	s.Subscribe(context.TODO(), func(job projection.Job) error {
//		events, errs, err := job.Events(job) // fetch all events of the Job
//		events, errs, err := job.Events(job, query.New(...)) // fetch events with filter
//		events, errs, err := job.EventsOf(job, "foo", "bar") // fetch events that belong to specific aggregates
//		events, errs, err := job.EventsFor(job, proj) // fetch events that would be applied to proj
//		tuples, errs, err := job.Aggregates(job) // extract aggregates from events
//		tuples, errs, err := job.Aggregates(job, "foo", "bar") // extract specific aggregates from events
//		id, err := job.Aggregate(job, "foo") // extract UUID of first aggregate with given name
//	})
//
// When the schedule is triggered by calling schedule.Trigger, a projection Job
// will be created and passed to apply.
func (schedule *Continuous) Subscribe(ctx context.Context, apply func(projection.Job) error, opts ...projection.SubscribeOption) (<-chan error, error) {
	cfg := projection.NewSubscription(opts...)

	events, errs, err := schedule.bus.Subscribe(ctx, schedule.eventNames...)
	if err != nil {
		return nil, fmt.Errorf("subscribe to %v events: %w", schedule.eventNames, err)
	}

	out := make(chan error)
	jobs := make(chan projection.Job)
	triggers := schedule.newTriggers()
	done := make(chan struct{})

	go func() {
		<-done
		schedule.removeTriggers(triggers)
	}()

	if cfg.Startup != nil {
		if err := schedule.applyStartupJob(ctx, cfg, jobs, apply); err != nil {
			return nil, fmt.Errorf("startup: %w", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go schedule.handleEvents(ctx, cfg, events, errs, jobs, out, &wg)
	go schedule.handleTriggers(ctx, cfg, triggers, jobs, out, &wg)
	go schedule.applyJobs(ctx, apply, jobs, out, done)

	go func() {
		wg.Wait()
		close(jobs)
	}()

	return out, nil
}

func (schedule *Continuous) handleEvents(
	ctx context.Context,
	sub projection.Subscription,
	events <-chan event.Event,
	errs <-chan error,
	jobs chan<- projection.Job,
	out chan<- error,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	fail := func(err error) {
		select {
		case <-ctx.Done():
		case out <- err:
		}
	}

	var mux sync.Mutex
	var buf []event.Event
	var debounce, debounceCap *time.Timer
	var jobCreated bool

	clearDebounce := func() {
		mux.Lock()
		defer mux.Unlock()

		jobCreated = false

		if debounce != nil {
			debounce.Stop()
			debounce = nil
		}

		if debounceCap != nil {
			debounceCap.Stop()
			debounceCap = nil
		}
	}

	defer clearDebounce()

	createJob := func() {
		defer clearDebounce()

		mux.Lock()
		defer mux.Unlock()

		if jobCreated {
			return
		}

		events := make([]event.Event, len(buf))
		copy(events, buf)

		job := schedule.newJob(
			ctx,
			sub,
			eventstore.New(events...),
			query.New(query.SortBy(event.SortTime, event.SortAsc)),
		)

		select {
		case <-ctx.Done():
		case jobs <- job:
		}

		buf = buf[:0]
		jobCreated = true
	}

	addEvent := func(evt event.Event) {
		clearDebounce()

		buf = append(buf, evt)

		if schedule.debounce <= 0 {
			createJob()
			return
		}

		mux.Lock()
		defer mux.Unlock()

		debounce = time.AfterFunc(schedule.debounce, createJob)

		if cap := schedule.computeDebounceCap(); cap > 0 {
			debounceCap = time.AfterFunc(cap, createJob)
		}
	}

	streams.ForEach(ctx, addEvent, fail, events, errs)
}

func (s *Continuous) computeDebounceCap() time.Duration {
	if s.debounceCap <= 0 {
		return 0
	}

	if s.debounceCapManuallySet {
		return s.debounceCap
	}

	if s.debounce <= defaultDebounceBarrier {
		return s.debounceCap
	}

	return s.debounce * 2
}
