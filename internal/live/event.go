package live

import (
	"bytes"
	"sync"
)

// Event is one Datastar server-sent event. A frame's events are shared by
// every stream of the board: they are formatted once, and only compressed
// per stream.
type Event struct {
	signals bool
	body    []byte
	once    sync.Once
	wire    []byte
}

// Elements is a datastar-patch-elements event (outer morph, by id).
func Elements(html []byte) *Event { return &Event{body: html} }

// Signals is a datastar-patch-signals event with a JSON merge patch.
func Signals(json []byte) *Event { return &Event{signals: true, body: json} }

// IsSignals tells the two kinds apart.
func (e *Event) IsSignals() bool { return e.signals }

// Body is the HTML or the JSON.
func (e *Event) Body() []byte { return e.body }

// Wire is the event as sent: one data line per line of the body.
func (e *Event) Wire() []byte {
	e.once.Do(func() {
		head, prefix := "event: datastar-patch-elements\n", "data: elements "
		if e.signals {
			head, prefix = "event: datastar-patch-signals\n", "data: signals "
		}
		b := make([]byte, 0, len(head)+len(e.body)+len(prefix)*(1+bytes.Count(e.body, []byte("\n")))+2)
		b = append(b, head...)
		if len(e.body) > 0 {
			for line := range bytes.SplitSeq(e.body, []byte("\n")) {
				b = append(b, prefix...)
				b = append(b, line...)
				b = append(b, '\n')
			}
		}
		e.wire = append(b, '\n')
	})
	return e.wire
}
