package clients

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sseEvent is one server-sent event: its name (the "event:" field) and its data.
type sseEvent struct {
	name string
	data string
}

// errStreamClosed means the server ended the stream normally.
var errStreamClosed = errors.New("event stream closed by the server")

// readEvents parses a text/event-stream body, the format of server-sent events
// (https://html.spec.whatwg.org/multipage/server-sent-events.html), and calls handle for
// each event. The format is line based:
//
//	: ping                       a comment; Tasuki streams send one every 15 s to keep the line alive
//	event: offer                 names the event (the default name is "message")
//	data: {"offer_id": "..."}    the payload; several data lines are joined with newlines
//	                             a blank line ends the event
//
// alive is called for every line, comments included, so the caller can tell the connection
// is still healthy. readEvents returns when the stream ends or handle returns an error.
func readEvents(r io.Reader, alive func(), handle func(sseEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20) // allow events up to 1 MiB
	var name string
	var data []string
	for scanner.Scan() {
		alive()
		line := scanner.Text() // the scanner already strips "\n" and "\r\n"
		switch {
		case line == "": // end of an event
			if len(data) > 0 {
				if name == "" {
					name = "message"
				}
				if err := handle(sseEvent{name: name, data: strings.Join(data, "\n")}); err != nil {
					return err
				}
			}
			name, data = "", nil
		case strings.HasPrefix(line, ":"): // a comment
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ") // one optional space after the colon
			switch field {
			case "event":
				name = value
			case "data":
				data = append(data, value)
			}
			// "id" and "retry" are part of the format, but Tasuki's streams do not use them.
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read event stream: %w", err)
	}
	return errStreamClosed
}
