package views

import (
	"bytes"
	"context"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"kanban/internal/app"
)

// The ARIA states that data-attr sets are the strings 'true' or 'false':
// data-attr writes a boolean true as an empty value, which assistive
// technology reads as false.
func TestARIAStatesAreStrings(t *testing.T) {
	b := &app.Board{ID: 1, Name: "Board", Lists: []app.List{{ID: 2, Name: "To do", Cards: []app.Card{{ID: 3, ListID: 2, Title: "Card"}}}}}
	c := &app.CardDetail{Card: app.Card{ID: 3, ListID: 2, Title: "Card", DueOn: "2026-10-13"}, BoardID: 1, ListName: "To do"}
	page := Page{Title: "Board", Assets: func(f string) string { return "/static/" + f }, User: &app.User{ID: 1, Name: "Anna"}}
	var out bytes.Buffer
	for _, comp := range []templ.Component{
		BoardPage(BoardShell{Page: page, Board: b, Tab: "tab-0000000000000001"}),
		Lane(b, 0, b.Lists[0], func(app.Card) string { return "" }),
		Detail(c, "", nil, time.Now()),
	} {
		if err := comp.Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
	}
	states := regexp.MustCompile(`data-attr:(aria-[a-z]+)="([^"]*)"`).FindAllStringSubmatch(out.String(), -1)
	if len(states) < 7 {
		t.Fatalf("%d ARIA states in the board, a lane and a card", len(states))
	}
	for _, s := range states {
		if expr := html.UnescapeString(s[2]); !strings.HasSuffix(expr, `? 'true' : 'false'`) {
			t.Errorf("%s is set to a boolean: %s", s[1], expr)
		}
	}
}
