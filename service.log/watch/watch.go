package watch

import (
	"context"
	"home-automation/libraries/go/errors"
	"home-automation/libraries/go/slog"
	"sync"

	"github.com/fsnotify/fsnotify"
	"home-automation/service.log/domain"
	"home-automation/service.log/repository"
)

// Watcher notifies subscribers of new events whenever the log file is written to
type Watcher struct {
	// LogDAO provides access to the log events
	LogRepository *repository.LogRepository

	watcher     *fsnotify.Watcher
	subscribers map[chan<- *domain.Event]*repository.LogQuery
	mux         sync.Mutex
}

// GetName returns the name "watcher"
func (w *Watcher) GetName() string {
	return "watcher"
}

// Start begins watching for log file changes and notifies subscribers accordingly
func (w *Watcher) Start() error {
	// Make sure the receiver struct has been initialised properly
	if w.LogRepository == nil {
		return errors.InternalService("LogRepository is not set")
	}
	if w.LogRepository.LogDirectory == "" {
		return errors.InternalService("Log directory is not set")
	}

	// Create an fsnotify watcher and attach to w so
	// that the Stop method can call Close() on it
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.Wrap(err, nil)
	}
	defer watcher.Close()
	w.watcher = watcher

	// Start watching the log file directory so we
	// are notified when new log files are created
	if err = watcher.Add(w.LogRepository.LogDirectory); err != nil {
		return errors.Wrap(err, nil)
	}
	slog.Info("Watching %s for changes", w.LogRepository.LogDirectory)

	for {
		select {
		case fileEvent, ok := <-watcher.Events:
			if !ok {
				// If the channel is closed then just exit silently
				// because Stop() was probably called
				return nil
			}

			// We'll get a write event if any file inside the directory is written to.
			// If the file isn't actually a log file we'll waste some work
			// trying to read new events but it's safe to do.
			if fileEvent.Op&fsnotify.Write != fsnotify.Write {
				continue
			}

			w.notifySubscribers()

		case err, ok := <-watcher.Errors:
			if !ok {
				// If the channel is closed then just exit silently
				// because Stop() was probably called
				return nil
			}

			// It's unclear what state the watcher will be in if we receive
			// any errors so just return, which will trigger Close()
			return errors.Wrap(err, nil)
		}
	}
}

// Stop stops watching for log file changes
func (w *Watcher) Stop(ctx context.Context) error {
	if w.watcher != nil {
		return w.watcher.Close()
	}

	return nil
}

// Subscribe starts sending all events that match the query over the given channel. The query
// will be updated with the a new SinceUUID value whenever events are published to the channel.
func (w *Watcher) Subscribe(c chan<- *domain.Event, q *repository.LogQuery) error {
	if q.SinceUUID == "" {
		return errors.InternalService("SinceUUID not set in subscriber query")
	}

	// Obtain a lock so we can write to the map
	w.mux.Lock()
	defer w.mux.Unlock()

	// Initialise the map if necessary
	if w.subscribers == nil {
		w.subscribers = make(map[chan<- *domain.Event]*repository.LogQuery)
	}

	// A channel is comparable so it's fine to use as a key
	w.subscribers[c] = q

	return nil
}

// Unsubscribe stops publishing events to the channel but does not close the channel
func (w *Watcher) Unsubscribe(c chan<- *domain.Event) {
	w.mux.Lock()
	defer w.mux.Unlock()
	delete(w.subscribers, c)
}

func (w *Watcher) notifySubscribers() {
	// Obtain a write lock before doing anything so that
	// we don't send duplicate events to the subscriber
	w.mux.Lock()
	defer w.mux.Unlock()

	for c, q := range w.subscribers {
		// Ensure that events are always published in order
		q.Reverse = false

		// Get all new events for this subscriber
		events, err := w.LogRepository.Find(q)
		if err != nil {
			slog.Error("Failed to get events for subscriber: %v", err)
			continue
		}

		// Send the events over the channel
		for _, event := range events {
			select {
			case c <- event: // Non-blocking write to the channel
			default: // Don't log otherwise we get a cycle of logs
			}
		}

		// Update the query for this subscriber
		if len(events) > 0 {
			// Events will always be in order so we can take the UUID of the last one
			q.SinceUUID = events[len(events)-1].UUID
		}
	}
}