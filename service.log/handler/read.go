package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jakewright/home-automation/libraries/go/errors"
	"github.com/jakewright/home-automation/libraries/go/request"
	"github.com/jakewright/home-automation/libraries/go/response"
	"github.com/jakewright/home-automation/libraries/go/slog"
	"github.com/jakewright/home-automation/service.log/domain"
	"github.com/jakewright/home-automation/service.log/repository"
	"github.com/jakewright/home-automation/service.log/watch"

	"github.com/gorilla/websocket"
)

const htmlTimeFormat = "2006-01-02T15:04"

type ReadHandler struct {
	TemplateDirectory string
	LogRepository     *repository.LogRepository
	Watcher           *watch.Watcher
}

type readRequest struct {
	Services  string `json:"services"`
	Severity  int    `json:"severity"`
	SinceTime string `json:"since_time"` // The HTML datetime-local element formats time weirdly so we need to unmarshal to a string
	UntilTime string `json:"until_time"`
	SinceUUID string `json:"since_uuid"`
	Reverse   bool   `json:"reverse"`
}

func (h *ReadHandler) DecodeBody(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	body := readRequest{}
	if err := request.Decode(r, &body); err != nil {
		response.WriteJSON(w, err)
		return
	}

	query, err := parseQuery(&body)
	if err != nil {
		slog.Error("Failed to parse options from body: %v", err)
		response.WriteJSON(w, err)
		return
	}

	metadata := map[string]string{
		"services":  strings.Join(query.Services, ", "),
		"severity":  query.Severity.String(),
		"sinceTime": query.SinceTime.Format(time.RFC3339),
		"untilTime": query.UntilTime.Format(time.RFC3339),
		"sinceUUID": query.SinceUUID,
		"reverse":   strconv.FormatBool(query.Reverse),
	}

	ctx := context.WithValue(r.Context(), "query", query)
	ctx = context.WithValue(ctx, "metadata", metadata)
	next(w, r.WithContext(ctx))
}

func (h *ReadHandler) HandleRead(w http.ResponseWriter, r *http.Request) {
	query := r.Context().Value("query").(*repository.LogQuery)
	metadata := r.Context().Value("metadata").(map[string]string)

	// Default to logs from the last hour
	if query.SinceTime.IsZero() {
		query.SinceTime = time.Now().Add(-1 * time.Hour)
	}
	if query.UntilTime.IsZero() {
		query.UntilTime = time.Now()
	}

	events, err := h.LogRepository.Find(query)
	if err != nil {
		slog.Error("Failed to find events: %v", err, metadata)
		response.WriteJSON(w, err)
		return
	}

	var lastUUID string

	if len(events) > 0 {
		if query.Reverse {
			lastUUID = events[0].UUID
		} else {
			lastUUID = events[len(events)-1].UUID
		}
	}

	formattedEvents := make([]*domain.FormattedEvent, len(events))
	for i, event := range events {
		formattedEvents[i] = event.Format()
	}

	rsp := struct {
		FormattedEvents []*domain.FormattedEvent
		Services        string
		Severity        int
		SinceTime       string
		UntilTime       string
		LastUUID        string
		Reverse         bool
	}{
		FormattedEvents: formattedEvents,
		Services:        strings.Join(query.Services, ", "),
		Severity:        int(query.Severity),
		SinceTime:       query.SinceTime.Format(htmlTimeFormat),
		UntilTime:       query.UntilTime.Format(htmlTimeFormat),
		LastUUID:        lastUUID,
		Reverse:         query.Reverse,
	}

	t, err := template.ParseFiles(path.Join(h.TemplateDirectory, "index.html"))
	if err != nil {
		slog.Error("Failed to parse template: %v", err)
		response.WriteJSON(w, err)
		return
	}

	var buf bytes.Buffer
	err = t.Execute(&buf, rsp)
	if err != nil {
		slog.Error("Failed to execute template: %v", err)
		response.WriteJSON(w, err)
		return
	}

	response.Write(w, buf)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool {
		return true
	},
}

func (h *ReadHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	query := r.Context().Value("query").(*repository.LogQuery)
	metadata := r.Context().Value("metadata").(map[string]string)

	// Upgrade the request to a WebSocket connection
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("Failed to create websocket upgrader: %v", err, metadata)
		return
	}
	defer ws.Close()

	// A loop must be started that reads and discards messages until a non-nil
	// error is received so that close, ping and pong messages are processed.
	// Close a channel to signal to the for loop below that the client has gone away.
	done := make(chan struct{})
	go func() {
		defer close(done)
		readLoop(ws)
	}()

	// Subscribe to new events that match the query in the request
	events := make(chan *domain.Event, 50)
	err = h.Watcher.Subscribe(events, query)
	if err != nil {
		slog.Error("Failed to subscribe to the watcher: %v", err, metadata)
		return
	}
	defer func() {
		h.Watcher.Unsubscribe(events)
	}()

	for {
		select {
		case event, ok := <-events:
			if !ok {
				slog.Error("Events channel unexpectedly closed")
				return
			}

			formattedEvent := event.Format()
			b, err := json.Marshal(formattedEvent)
			if err != nil {
				slog.Error("Failed to marshal event: %v", err, metadata)
				continue
			}

			if err := ws.WriteMessage(websocket.TextMessage, b); err != nil {
				slog.Error("Failed to write message to websocket: %v", err, metadata)
				return
			}
		case <-done:
			// The WebSocket is closed so silently return
			return
		}
	}
}

func parseQuery(body *readRequest) (*repository.LogQuery, error) {
	var services []string
	if body.Services != "" {
		services = strings.Split(strings.Replace(body.Services, " ", "", -1), ",")
	}

	severity := slog.Severity(body.Severity)

	var err error
	var sinceTime, untilTime time.Time

	if body.SinceTime != "" {
		sinceTime, err = time.Parse(htmlTimeFormat, body.SinceTime)
		if err != nil {
			return nil, errors.Wrap(err, nil)
		}
	}

	if body.UntilTime != "" {
		untilTime, err = time.Parse(htmlTimeFormat, body.UntilTime)
		if err != nil {
			return nil, errors.Wrap(err, nil)
		}
	}

	return &repository.LogQuery{
		Services:  services,
		Severity:  severity,
		SinceTime: sinceTime,
		UntilTime: untilTime,
		SinceUUID: body.SinceUUID,
		Reverse:   body.Reverse,
	}, nil
}

func readLoop(c *websocket.Conn) {
	for {
		if _, _, err := c.NextReader(); err != nil {
			c.Close()
			break
		}
	}
}
