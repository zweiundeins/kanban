// Package views renders every page and fragment. Shared fragments (the
// board, a card's dialog) never contain anything about the viewer: the
// same bytes go to everyone on the board.
package views

import (
	"fmt"

	"kanban/internal/app"
	"strconv"
	"strings"
	"time"
)

// Assets are the static files' URL prefix (it changes with their content).
type Assets struct {
	Prefix string
}

func (a Assets) URL(file string) string { return a.Prefix + "/" + file }

// opID makes a new op id in the browser.
const opID = `Date.now().toString(36) + Math.random().toString(36).slice(2, 7)`

// Act is a Datastar expression that posts an action. key names what is
// pending meanwhile ($_pend[key] holds the op id, $_acks[op] its outcome
// once the stream brings it); payload is extra JS object members, like
// "title: $_in.l4". keyExpr is a JS expression for the key.
func Act(url, keyExpr, payload string) string {
	if payload != "" {
		payload = ", " + payload
	}
	// One comma expression, so callers can write "cond && (" + Act(...) + ")".
	return fmt.Sprintf(`($_pend[%[2]s] = %[3]s, @post(%[1]s, {payload: {tab: $_tab, op: $_pend[%[2]s]%[4]s}, requestCancellation: 'disabled'}))`,
		url, keyExpr, opID, payload)
}

// ActKey is Act with a fixed key.
func ActKey(url, key, payload string) string { return Act(js(url), js(key), payload) }

// UI posts a command about this tab's view (which card is open), no op.
func UI(url, payload string) string {
	return fmt.Sprintf(`@post(%s, {payload: {tab: $_tab, %s}, requestCancellation: 'disabled'})`, js(url), payload)
}

// Pending is true while key's action waits for its outcome.
func Pending(key string) string {
	return fmt.Sprintf(`!!($_pend[%[1]s] && !$_acks[$_pend[%[1]s]])`, js(key))
}

// Refused is true when key's last action was refused or failed.
func Refused(key string) string {
	return fmt.Sprintf(`!!($_pend[%[1]s] && $_acks[$_pend[%[1]s]] && $_acks[$_pend[%[1]s]] !== 'ok')`, js(key))
}

// Done is true when key's last action succeeded.
func Done(key string) string {
	return fmt.Sprintf(`!!($_pend[%[1]s] && $_acks[$_pend[%[1]s]] === 'ok')`, js(key))
}

// js quotes s as a JS string literal.
func js(s string) string {
	return strconv.Quote(s)
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

func boardURL(b int64, rest string) string { return "/boards/" + id(b) + rest }

// Due describes a due date for a card's badge.
type Due struct {
	Text  string
	Class string // done, overdue, soon, ""
	Full  string
}

// DueOf formats a YYYY-MM-DD due date relative to today.
func DueOf(day string, done bool, today time.Time) Due {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return Due{}
	}
	d := Due{Text: t.Format("Jan 2"), Full: t.Format("Monday, 2 January 2006")}
	if t.Year() != today.Year() {
		d.Text = t.Format("Jan 2, 2006")
	}
	y, m, dd := today.Date()
	days := int(t.Sub(time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	switch {
	case done:
		d.Class = "done"
		d.Full += ", done"
	case days < 0:
		d.Class = "overdue"
		d.Full += ", overdue"
	case days <= 1:
		d.Class = "soon"
		d.Full += ", due soon"
	}
	return d
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func ago(unix int64, now time.Time) string {
	d := now.Sub(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute", "minutes") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour", "hours") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day", "days") + " ago"
	}
	return time.Unix(unix, 0).UTC().Format("2 Jan 2006")
}

func initials(name string) string {
	out := ""
	for _, p := range strings.Fields(name) {
		r := []rune(p)
		out += strings.ToUpper(string(r[0]))
		if len([]rune(out)) == 2 {
			break
		}
	}
	return out
}

// ImportMap is the inline import map's text (the CSP allows it by hash).
func ImportMap(prefix string) string {
	return `{"imports":{"datastar":"` + prefix + `/vendor/datastar-rocket.js"}}`
}

// SpeculationRules prefetch boards (not card URLs: the board is the same page).
const SpeculationRules = `{"prefetch":[{"where":{"and":[{"href_matches":"/boards/*"},{"not":{"href_matches":"/boards/*/cards/*"}}]},"eagerness":"moderate"}]}`

// viewing names who has a card open ("Anna and Ben have this card open").
func viewing(users []app.User) string {
	names := make([]string, len(users))
	for i, u := range users {
		names[i] = u.FirstName()
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " has this card open"
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " have this card open"
}
