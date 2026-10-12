// Package live is kanban's side of lith's live hub: the frames of a board
// and of a tab, who is on which board with which card open, and what each
// stream sends of a frame that every stream shares.
package live

import (
	"bytes"
	"sync"

	"github.com/zweiundeins/lith/sse"
)

// Board is one rendering of a board for everyone on it: its lanes, the
// dialogs of the cards open in any tab, and the board version it shows.
// Its events are formatted once, by Prepare, and shared by every stream.
type Board struct {
	Version int64
	// Lanes is patched as one element, but a stream gets a lane in full
	// only when it hasn't sent that exact lane before: otherwise the lane's
	// shell, which the morph skips. A change in one lane then costs the
	// browser one lane, not the whole board.
	Lanes *Keyed
	// Details are the dialogs of the cards open in any tab, by card id;
	// Closed is the dialog when no card is open.
	Details map[int64][]byte
	Closed  []byte

	seq     uint64
	full    func() []byte    // every lane, for a stream that has none
	delta   []byte           // the lanes that changed since the frame before; nil: none
	base    uint64           // seq of that frame
	details map[int64][]byte // the dialogs as events
	closed  []byte
}

// Prepare formats the frame's events against prev, the frame before it
// (nil: none). It reports false when the frame shows exactly what prev
// does, which then stays the board's frame.
func (f *Board) Prepare(prev *Board) bool {
	if prev != nil && f.Version == prev.Version && f.Lanes.same(prev.Lanes) && bytes.Equal(f.Closed, prev.Closed) &&
		len(f.Details) == len(prev.Details) && sameDetails(f.Details, prev.Details) {
		return false
	}
	f.full = sync.OnceValue(func() []byte { return sse.PatchElements(f.Lanes.Full()) })
	f.details = make(map[int64][]byte, len(f.Details))
	f.closed = sse.PatchElements(f.Closed)
	if prev == nil {
		f.seq = 1
		for card, d := range f.Details {
			f.details[card] = sse.PatchElements(d)
		}
		return true
	}
	f.seq, f.base = prev.seq+1, prev.seq
	if b := f.Lanes.against(prev.Lanes.Children); b != nil {
		f.delta = sse.PatchElements(b)
	}
	for card, d := range f.Details {
		if old, ok := prev.Details[card]; ok && bytes.Equal(old, d) {
			f.details[card] = prev.details[card]
		} else {
			f.details[card] = sse.PatchElements(d)
		}
	}
	if bytes.Equal(f.Closed, prev.Closed) {
		f.closed = prev.closed
	}
	return true
}

func sameDetails(a, b map[int64][]byte) bool {
	for card, d := range a {
		if old, ok := b[card]; !ok || !bytes.Equal(old, d) {
			return false
		}
	}
	return true
}

// Keyed is an element with keyed children (the board and its lanes).
type Keyed struct {
	Open, Close []byte
	Children    []Child
}

// Child is one keyed child: its markup, and the shell sent in its place
// when the stream already has it.
type Child struct {
	Key         int64
	HTML, Shell []byte
}

// Full is the element with every child in full (the page's first render).
func (k *Keyed) Full() []byte {
	n := len(k.Open) + len(k.Close)
	for _, c := range k.Children {
		n += len(c.HTML)
	}
	out := make([]byte, 0, n)
	out = append(out, k.Open...)
	for _, c := range k.Children {
		out = append(out, c.HTML...)
	}
	return append(out, k.Close...)
}

func (k *Keyed) same(o *Keyed) bool {
	if len(k.Children) != len(o.Children) {
		return false
	}
	for i, c := range k.Children {
		if c.Key != o.Children[i].Key || !bytes.Equal(c.HTML, o.Children[i].HTML) {
			return false
		}
	}
	return true
}

// against is the element for a stream that has sent the children had:
// the children it has unchanged go as shells. Nil when nothing changed.
func (k *Keyed) against(had []Child) []byte {
	prev := make(map[int64][]byte, len(had))
	for _, c := range had {
		prev[c.Key] = c.HTML
	}
	changed := len(had) != len(k.Children)
	n := len(k.Open) + len(k.Close)
	for _, c := range k.Children {
		if h, ok := prev[c.Key]; ok && bytes.Equal(h, c.HTML) {
			n += len(c.Shell)
		} else {
			n += len(c.HTML)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	out := make([]byte, 0, n)
	out = append(out, k.Open...)
	for _, c := range k.Children {
		if h, ok := prev[c.Key]; ok && bytes.Equal(h, c.HTML) {
			out = append(out, c.Shell...)
		} else {
			out = append(out, c.HTML...)
		}
	}
	return append(out, k.Close...)
}
