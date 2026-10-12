package main

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/mikemackintosh/mcplocker-provider-google/internal/id"
	"github.com/mikemackintosh/mcplocker-provider-google/internal/provider"
)

const calBase = apiBase + "/calendar/v3"

// The knowledge these tools carry, so the agent does not have to:
//
//   - The API does not put the creator on the guest list. An event you
//     insert on your own calendar has you as organizer and nobody as an
//     attendee, which means no invitation state for you and, on shared
//     calendars, a meeting you are not in. create_event adds you, accepted,
//     unless told not to.
//   - Nothing is emailed unless sendUpdates says so. The default here is
//     "all" when there are guests and "none" when there are not.
//   - A Google Meet link is a conferenceData.createRequest and only takes
//     effect with conferenceDataVersion=1 on the request.
//   - PATCH replaces the attendee list wholesale, so update_event merges
//     add/remove against what is there; respond_to_event does the same to
//     set one attendee's responseStatus.
//   - An all-day event's end date is exclusive: a one-day event on the 3rd
//     ends on the 4th. The tools take inclusive dates and add the day.
//   - A time without an offset is in the calendar's own time zone, read
//     from the user's settings when the caller gives none.

var calendarTools = []provider.Tool{
	{Name: "list_calendars", Title: "List calendars",
		Description: "Calendars on your list, with id, summary, time zone, access role and whether it is the primary. Use the id with the other tools; 'primary' always means your own." + pagingNote,
		InputSchema: schema(`{"type":"object","properties":{
			"max_results":{"type":"integer","minimum":1,"maximum":250,"default":100},
			"page_token":{"type":"string"}}}`),
		Annotations: readOnly},
	{Name: "list_events", Title: "List calendar events",
		Description: "List events on a calendar between two RFC 3339 timestamps, recurring events expanded into instances, ordered by start. All-day events have start.date/end.date; timed ones start.dateTime/end.dateTime." + pagingNote,
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_id":{"type":"string","default":"primary"},
			"time_min":{"type":"string","format":"date-time"},
			"time_max":{"type":"string","format":"date-time"},
			"max_results":{"type":"integer","minimum":1,"maximum":2500,"default":250},
			"query":{"type":"string","description":"Free-text match on summary, description, location, attendees."},
			"page_token":{"type":"string"}},"required":["time_min","time_max"]}`),
		Annotations: readOnly},
	{Name: "get_event", Title: "Get an event",
		Description: "One event with everything the API holds: attendees and their responses, conference (Meet) link, recurrence, reminders, htmlLink.",
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_id":{"type":"string","default":"primary"},
			"event_id":{"type":"string"}},"required":["event_id"]}`),
		Annotations: readOnly},
	{Name: "find_free_time", Title: "Find free time",
		Description: "Busy intervals for calendars between two timestamps, so a caller can compute free slots. Only events that block time count; all-day and 'free' events do not.",
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_ids":{"type":"array","items":{"type":"string"},"default":["primary"]},
			"time_min":{"type":"string","format":"date-time"},
			"time_max":{"type":"string","format":"date-time"}},"required":["time_min","time_max"]}`),
		Annotations: readOnly},
	{Name: "create_event", Title: "Create an event or meeting",
		Description: "Create an event. You are added as an attendee (accepted) unless add_self is false, because the API does not do that and a meeting without its creator on the guest list shows no response state. " +
			"Invitations are emailed per send_updates: default 'all' when there are attendees, 'none' otherwise. google_meet adds a Meet link. " +
			"Times: RFC 3339 with offset, or without an offset in time_zone (default: your calendar's zone); a date (YYYY-MM-DD) makes an all-day event and end is inclusive. " +
			"Returns the event with id, htmlLink and hangoutLink.",
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_id":{"type":"string","default":"primary"},
			"summary":{"type":"string","description":"Title."},
			"description":{"type":"string"},
			"location":{"type":"string"},
			"start":{"type":"string","description":"RFC 3339 date-time, or YYYY-MM-DD for all-day."},
			"end":{"type":"string","description":"Same form as start. For all-day, inclusive; omit for a single day."},
			"time_zone":{"type":"string","description":"IANA zone for times without an offset, e.g. America/Los_Angeles. Default: your calendar's."},
			"attendees":{"type":"array","items":{"type":"string","format":"email"},"description":"Guest emails."},
			"optional_attendees":{"type":"array","items":{"type":"string","format":"email"}},
			"add_self":{"type":"boolean","default":true,"description":"Add you as an accepted attendee."},
			"google_meet":{"type":"boolean","default":false,"description":"Attach a Google Meet link."},
			"send_updates":{"type":"string","enum":["all","externalOnly","none"],"description":"Who is emailed. Default: all if attendees, else none."},
			"recurrence":{"type":"array","items":{"type":"string"},"description":"RFC 5545 lines, e.g. RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=10"},
			"reminder_minutes":{"type":"array","items":{"type":"integer","minimum":0}, "description":"Popup reminders, minutes before. Omit for the calendar's defaults."},
			"visibility":{"type":"string","enum":["default","public","private"]},
			"transparency":{"type":"string","enum":["opaque","transparent"],"description":"opaque blocks time (busy); transparent shows as free."},
			"guests_can_modify":{"type":"boolean"},
			"guests_can_invite_others":{"type":"boolean"},
			"guests_can_see_other_guests":{"type":"boolean"},
			"color_id":{"type":"string","description":"1–11, the calendar UI palette."}},
			"required":["summary","start"]}`),
		Annotations: writes},
	{Name: "quick_add_event", Title: "Quick-add an event from text",
		Description: "Create an event from natural language the way the Calendar box does ('Lunch with Sam Friday 12:30'). Google parses it; check the returned start/end. No attendees are parsed from text — use create_event for a meeting.",
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_id":{"type":"string","default":"primary"},
			"text":{"type":"string"}},"required":["text"]}`),
		Annotations: writes},
	{Name: "update_event", Title: "Update an event",
		Description: "Change fields of an event; only what you pass changes. Attendees: add_attendees and remove_attendees merge with the current list (the API would otherwise replace it); attendees replaces it outright. " +
			"send_updates defaults to 'all' when the guest list or time changed, else 'none'. google_meet: true attaches a Meet link if there is none.",
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_id":{"type":"string","default":"primary"},
			"event_id":{"type":"string"},
			"summary":{"type":"string"},
			"description":{"type":"string"},
			"location":{"type":"string"},
			"start":{"type":"string"},
			"end":{"type":"string"},
			"time_zone":{"type":"string"},
			"attendees":{"type":"array","items":{"type":"string","format":"email"},"description":"Replace the whole guest list."},
			"add_attendees":{"type":"array","items":{"type":"string","format":"email"}},
			"remove_attendees":{"type":"array","items":{"type":"string","format":"email"}},
			"google_meet":{"type":"boolean"},
			"send_updates":{"type":"string","enum":["all","externalOnly","none"]},
			"recurrence":{"type":"array","items":{"type":"string"}},
			"reminder_minutes":{"type":"array","items":{"type":"integer","minimum":0}},
			"visibility":{"type":"string","enum":["default","public","private"]},
			"transparency":{"type":"string","enum":["opaque","transparent"]},
			"guests_can_modify":{"type":"boolean"},
			"guests_can_invite_others":{"type":"boolean"},
			"guests_can_see_other_guests":{"type":"boolean"},
			"color_id":{"type":"string"}},
			"required":["event_id"]}`),
		Annotations: idempotentW},
	{Name: "respond_to_event", Title: "Respond to an invitation",
		Description: "Set your own response on an event you were invited to: accepted, declined or tentative, with an optional note the organizer sees. Fails clearly if you are not on the guest list (an organizer who is not an attendee has nothing to respond to).",
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_id":{"type":"string","default":"primary"},
			"event_id":{"type":"string"},
			"response":{"type":"string","enum":["accepted","declined","tentative"]},
			"comment":{"type":"string"},
			"send_updates":{"type":"string","enum":["all","externalOnly","none"],"default":"all"}},
			"required":["event_id","response"]}`),
		Annotations: idempotentW},
	{Name: "move_event", Title: "Move an event to another calendar",
		Description: "Move an event you organize to a different calendar you can write to. Guests keep their invitations.",
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_id":{"type":"string","default":"primary"},
			"event_id":{"type":"string"},
			"destination_calendar_id":{"type":"string"},
			"send_updates":{"type":"string","enum":["all","externalOnly","none"],"default":"none"}},
			"required":["event_id","destination_calendar_id"]}`),
		Annotations: idempotentW},
	{Name: "delete_event", Title: "Cancel and delete an event",
		Description: "Delete an event. With guests, send_updates 'all' (the default) emails the cancellation; 'none' removes it silently. Cannot be undone from here.",
		InputSchema: schema(`{"type":"object","properties":{
			"calendar_id":{"type":"string","default":"primary"},
			"event_id":{"type":"string"},
			"send_updates":{"type":"string","enum":["all","externalOnly","none"],"default":"all"}},
			"required":["event_id"]}`),
		Annotations: destructive},
}

// eventArgs is the union of create_event and update_event input.
type eventArgs struct {
	CalendarID         string   `json:"calendar_id"`
	EventID            string   `json:"event_id"`
	Summary            *string  `json:"summary"`
	Description        *string  `json:"description"`
	Location           *string  `json:"location"`
	Start              string   `json:"start"`
	End                string   `json:"end"`
	TimeZone           string   `json:"time_zone"`
	Attendees          []string `json:"attendees"`
	OptionalAttendees  []string `json:"optional_attendees"`
	AddAttendees       []string `json:"add_attendees"`
	RemoveAttendees    []string `json:"remove_attendees"`
	AddSelf            *bool    `json:"add_self"`
	GoogleMeet         *bool    `json:"google_meet"`
	SendUpdates        string   `json:"send_updates"`
	Recurrence         []string `json:"recurrence"`
	ReminderMinutes    []int    `json:"reminder_minutes"`
	Visibility         string   `json:"visibility"`
	Transparency       string   `json:"transparency"`
	GuestsCanModify    *bool    `json:"guests_can_modify"`
	GuestsCanInvite    *bool    `json:"guests_can_invite_others"`
	GuestsCanSeeOthers *bool    `json:"guests_can_see_other_guests"`
	ColorID            string   `json:"color_id"`
}

func (a *eventArgs) calendar() string {
	if a.CalendarID == "" {
		return "primary"
	}
	return a.CalendarID
}

func (b *Bundle) callCalendar(ctx context.Context, hc Doer, req provider.CallRequest) (provider.CallResult, error) {
	switch req.Tool {
	case "list_calendars":
		var a struct {
			MaxResults int `json:"max_results"`
			pageArgs
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		// The field mask must name nextPageToken, or it is never sent.
		q := query{"fields": "nextPageToken,items(id,summary,description,timeZone,accessRole,primary,selected)"}
		return b.get(ctx, hc, calBase+"/users/me/calendarList?"+paged(q, "maxResults", a.MaxResults, 250, a.PageToken).encode(), "items")
	case "list_events":
		var a struct {
			CalendarID string `json:"calendar_id"`
			TimeMin    string `json:"time_min"`
			TimeMax    string `json:"time_max"`
			MaxResults int    `json:"max_results"`
			Query      string `json:"query"`
			pageArgs
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.CalendarID == "" {
			a.CalendarID = "primary"
		}
		q := query{"timeMin": a.TimeMin, "timeMax": a.TimeMax, "singleEvents": "true", "orderBy": "startTime"}
		if a.Query != "" {
			q["q"] = a.Query
		}
		return b.get(ctx, hc, calBase+"/calendars/"+pathEsc(a.CalendarID)+"/events?"+paged(q, "maxResults", a.MaxResults, 2500, a.PageToken).encode(), "items")
	case "get_event":
		var a eventArgs
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		return result(b.call(ctx, hc, http.MethodGet, eventURL(a.calendar(), a.EventID, nil), nil))
	case "find_free_time":
		var a struct {
			CalendarIDs []string `json:"calendar_ids"`
			TimeMin     string   `json:"time_min"`
			TimeMax     string   `json:"time_max"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if len(a.CalendarIDs) == 0 {
			a.CalendarIDs = []string{"primary"}
		}
		items := make([]object, 0, len(a.CalendarIDs))
		for _, cid := range a.CalendarIDs {
			items = append(items, object{"id": cid})
		}
		return result(b.call(ctx, hc, http.MethodPost, calBase+"/freeBusy", object{"timeMin": a.TimeMin, "timeMax": a.TimeMax, "items": items}))
	case "create_event":
		var a eventArgs
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		return b.createEvent(ctx, hc, a)
	case "quick_add_event":
		var a struct {
			CalendarID string `json:"calendar_id"`
			Text       string `json:"text"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.CalendarID == "" {
			a.CalendarID = "primary"
		}
		if strings.TrimSpace(a.Text) == "" {
			return errorf("text is required"), nil
		}
		return result(b.call(ctx, hc, http.MethodPost, calBase+"/calendars/"+pathEsc(a.CalendarID)+"/events/quickAdd?"+query{"text": a.Text}.encode(), nil))
	case "update_event":
		var a eventArgs
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		return b.updateEvent(ctx, hc, a)
	case "respond_to_event":
		var a struct {
			eventArgs
			Response string `json:"response"`
			Comment  string `json:"comment"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		return b.respondToEvent(ctx, hc, a.eventArgs, a.Response, a.Comment)
	case "move_event":
		var a struct {
			eventArgs
			Destination string `json:"destination_calendar_id"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.Destination == "" {
			return errorf("destination_calendar_id is required"), nil
		}
		su := a.SendUpdates
		if su == "" {
			su = "none"
		}
		return result(b.call(ctx, hc, http.MethodPost, eventURL(a.calendar(), a.EventID, nil)+"/move?"+query{"destination": a.Destination, "sendUpdates": su}.encode(), nil))
	case "delete_event":
		var a eventArgs
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.EventID == "" {
			return errorf("event_id is required"), nil
		}
		su := a.SendUpdates
		if su == "" {
			su = "all"
		}
		if _, err := b.call(ctx, hc, http.MethodDelete, eventURL(a.calendar(), a.EventID, query{"sendUpdates": su}), nil); err != nil {
			return result(nil, err)
		}
		return objResult(object{"deleted": true, "event_id": a.EventID, "calendar_id": a.calendar(), "send_updates": su}), nil
	}
	return provider.CallResult{}, fmt.Errorf("google/calendar: unknown tool %q", req.Tool)
}

// eventURL is the event resource, with optional query. The base path ends
// at the event so callers can append /move.
func eventURL(calendar, eventID string, q query) string {
	u := calBase + "/calendars/" + pathEsc(calendar) + "/events/" + pathEsc(eventID)
	if len(q) > 0 {
		u += "?" + q.encode()
	}
	return u
}

// selfEmail is the signed-in account: the primary calendar's id is its
// address, and that is how the API wants an attendee named.
func (b *Bundle) selfEmail(ctx context.Context, hc Doer) (string, error) {
	o, err := b.callObj(ctx, hc, http.MethodGet, calBase+"/calendars/primary?fields=id", nil)
	if err != nil {
		return "", err
	}
	s, _ := o["id"].(string)
	if s == "" {
		return "", fmt.Errorf("google/calendar: could not read your address from the primary calendar")
	}
	return s, nil
}

// userTimeZone is the calendar's own zone, for times given without one.
func (b *Bundle) userTimeZone(ctx context.Context, hc Doer) string {
	o, err := b.callObj(ctx, hc, http.MethodGet, calBase+"/users/me/settings/timezone", nil)
	if err != nil {
		return "UTC"
	}
	if v, _ := o["value"].(string); v != "" {
		return v
	}
	return "UTC"
}

// eventTime turns the tool's start/end into the API's shape. A date is an
// all-day event; a time keeps its offset if it has one and otherwise is
// stated in tz. For all-day ends, inclusive in, exclusive out.
func eventTime(s, tz string, isEnd bool) (object, bool, error) {
	s = strings.TrimSpace(s)
	if len(s) == 10 {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return nil, false, fmt.Errorf("%q is not YYYY-MM-DD or RFC 3339", s)
		}
		if isEnd {
			d = d.AddDate(0, 0, 1)
		}
		return object{"date": d.Format("2006-01-02")}, true, nil
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return object{"dateTime": s, "timeZone": tz}, false, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return object{"dateTime": t.Format("2006-01-02T15:04:05"), "timeZone": tz}, false, nil
	}
	if t, err := time.Parse("2006-01-02T15:04", s); err == nil {
		return object{"dateTime": t.Format("2006-01-02T15:04:05"), "timeZone": tz}, false, nil
	}
	return nil, false, fmt.Errorf("%q is not YYYY-MM-DD or RFC 3339", s)
}

func validEmails(list []string) error {
	for _, e := range list {
		if _, err := mail.ParseAddress(e); err != nil {
			return fmt.Errorf("%q is not an email address", e)
		}
	}
	return nil
}

func attendeeList(required, optional []string) []object {
	out := make([]object, 0, len(required)+len(optional))
	for _, e := range required {
		out = append(out, object{"email": e})
	}
	for _, e := range optional {
		out = append(out, object{"email": e, "optional": true})
	}
	return out
}

// applyEventFields writes the optional fields into an event body.
func applyEventFields(ev object, a eventArgs) {
	if a.Summary != nil {
		ev["summary"] = *a.Summary
	}
	if a.Description != nil {
		ev["description"] = *a.Description
	}
	if a.Location != nil {
		ev["location"] = *a.Location
	}
	if a.Recurrence != nil {
		ev["recurrence"] = a.Recurrence
	}
	if a.ReminderMinutes != nil {
		ov := make([]object, 0, len(a.ReminderMinutes))
		for _, m := range a.ReminderMinutes {
			ov = append(ov, object{"method": "popup", "minutes": m})
		}
		ev["reminders"] = object{"useDefault": false, "overrides": ov}
	}
	if a.Visibility != "" {
		ev["visibility"] = a.Visibility
	}
	if a.Transparency != "" {
		ev["transparency"] = a.Transparency
	}
	if a.GuestsCanModify != nil {
		ev["guestsCanModify"] = *a.GuestsCanModify
	}
	if a.GuestsCanInvite != nil {
		ev["guestsCanInviteOthers"] = *a.GuestsCanInvite
	}
	if a.GuestsCanSeeOthers != nil {
		ev["guestsCanSeeOtherGuests"] = *a.GuestsCanSeeOthers
	}
	if a.ColorID != "" {
		ev["colorId"] = a.ColorID
	}
}

func meetRequest() object {
	return object{"createRequest": object{"requestId": id.New().String(), "conferenceSolutionKey": object{"type": "hangoutsMeet"}}}
}

func (b *Bundle) createEvent(ctx context.Context, hc Doer, a eventArgs) (provider.CallResult, error) {
	if a.Summary == nil || strings.TrimSpace(*a.Summary) == "" {
		return errorf("summary is required"), nil
	}
	if a.Start == "" {
		return errorf("start is required"), nil
	}
	if err := validEmails(append(append([]string{}, a.Attendees...), a.OptionalAttendees...)); err != nil {
		return errResult(err), nil
	}
	tz := a.TimeZone
	if tz == "" {
		tz = b.userTimeZone(ctx, hc)
	}
	start, allDay, err := eventTime(a.Start, tz, false)
	if err != nil {
		return errorf("start: %v", err), nil
	}
	endIn := a.End
	if endIn == "" {
		if !allDay {
			return errorf("end is required for a timed event"), nil
		}
		endIn = a.Start
	}
	end, endAllDay, err := eventTime(endIn, tz, true)
	if err != nil {
		return errorf("end: %v", err), nil
	}
	if allDay != endAllDay {
		return errorf("start and end must both be dates or both be times"), nil
	}
	if !allDay {
		st, _ := time.Parse(time.RFC3339, a.Start)
		en, _ := time.Parse(time.RFC3339, a.End)
		if !st.IsZero() && !en.IsZero() && !en.After(st) {
			return errorf("end must be after start"), nil
		}
	}
	ev := object{"start": start, "end": end}
	applyEventFields(ev, a)
	attendees := attendeeList(a.Attendees, a.OptionalAttendees)
	if a.AddSelf == nil || *a.AddSelf {
		me, err := b.selfEmail(ctx, hc)
		if err != nil {
			return result(nil, err)
		}
		if !hasEmail(attendees, me) {
			attendees = append(attendees, object{"email": me, "responseStatus": "accepted", "self": true})
		}
	}
	if len(attendees) > 0 {
		ev["attendees"] = attendees
	}
	q := query{}
	if a.GoogleMeet != nil && *a.GoogleMeet {
		ev["conferenceData"] = meetRequest()
		q["conferenceDataVersion"] = "1"
	}
	su := a.SendUpdates
	if su == "" {
		if len(a.Attendees)+len(a.OptionalAttendees) > 0 {
			su = "all"
		} else {
			su = "none"
		}
	}
	q["sendUpdates"] = su
	raw, err := b.call(ctx, hc, http.MethodPost, calBase+"/calendars/"+pathEsc(a.calendar())+"/events?"+q.encode(), ev)
	return result(raw, err)
}

func hasEmail(list []object, email string) bool {
	for _, at := range list {
		if e, _ := at["email"].(string); strings.EqualFold(e, email) {
			return true
		}
	}
	return false
}

func (b *Bundle) updateEvent(ctx context.Context, hc Doer, a eventArgs) (provider.CallResult, error) {
	if a.EventID == "" {
		return errorf("event_id is required"), nil
	}
	if err := validEmails(append(append(append([]string{}, a.Attendees...), a.AddAttendees...), a.RemoveAttendees...)); err != nil {
		return errResult(err), nil
	}
	patch := object{}
	applyEventFields(patch, a)
	guestsChanged, timeChanged := false, false
	if a.Start != "" || a.End != "" {
		tz := a.TimeZone
		if tz == "" {
			tz = b.userTimeZone(ctx, hc)
		}
		// Both ends move together: changing one of a timed pair to a date
		// would be refused by the API, so the tool requires both.
		if a.Start == "" || a.End == "" {
			return errorf("pass start and end together"), nil
		}
		start, allDay, err := eventTime(a.Start, tz, false)
		if err != nil {
			return errorf("start: %v", err), nil
		}
		end, endAllDay, err := eventTime(a.End, tz, true)
		if err != nil {
			return errorf("end: %v", err), nil
		}
		if allDay != endAllDay {
			return errorf("start and end must both be dates or both be times"), nil
		}
		patch["start"], patch["end"] = start, end
		timeChanged = true
	}
	switch {
	case a.Attendees != nil:
		patch["attendees"] = attendeeList(a.Attendees, nil)
		guestsChanged = true
	case len(a.AddAttendees) > 0 || len(a.RemoveAttendees) > 0:
		// PATCH replaces the list, so read it and merge.
		cur, err := b.callObj(ctx, hc, http.MethodGet, eventURL(a.calendar(), a.EventID, query{"fields": "attendees"}), nil)
		if err != nil {
			return result(nil, err)
		}
		list := []object{}
		if arr, ok := cur["attendees"].([]any); ok {
			for _, x := range arr {
				if at, ok := x.(object); ok {
					drop := false
					for _, r := range a.RemoveAttendees {
						if e, _ := at["email"].(string); strings.EqualFold(e, r) {
							drop = true
						}
					}
					if !drop {
						list = append(list, at)
					}
				}
			}
		}
		for _, e := range a.AddAttendees {
			if !hasEmail(list, e) {
				list = append(list, object{"email": e})
			}
		}
		patch["attendees"] = list
		guestsChanged = true
	}
	q := query{}
	if a.GoogleMeet != nil && *a.GoogleMeet {
		patch["conferenceData"] = meetRequest()
		q["conferenceDataVersion"] = "1"
	}
	if len(patch) == 0 {
		return errorf("nothing to change"), nil
	}
	su := a.SendUpdates
	if su == "" {
		if guestsChanged || timeChanged {
			su = "all"
		} else {
			su = "none"
		}
	}
	q["sendUpdates"] = su
	return result(b.call(ctx, hc, http.MethodPatch, eventURL(a.calendar(), a.EventID, q), patch))
}

func (b *Bundle) respondToEvent(ctx context.Context, hc Doer, a eventArgs, response, comment string) (provider.CallResult, error) {
	if a.EventID == "" {
		return errorf("event_id is required"), nil
	}
	switch response {
	case "accepted", "declined", "tentative":
	default:
		return errorf("response must be accepted, declined or tentative"), nil
	}
	cur, err := b.callObj(ctx, hc, http.MethodGet, eventURL(a.calendar(), a.EventID, query{"fields": "attendees,organizer"}), nil)
	if err != nil {
		return result(nil, err)
	}
	me, err := b.selfEmail(ctx, hc)
	if err != nil {
		return result(nil, err)
	}
	list := []object{}
	found := false
	if arr, ok := cur["attendees"].([]any); ok {
		for _, x := range arr {
			at, ok := x.(object)
			if !ok {
				continue
			}
			e, _ := at["email"].(string)
			if self, _ := at["self"].(bool); self || strings.EqualFold(e, me) {
				at["responseStatus"] = response
				if comment != "" {
					at["comment"] = comment
				}
				found = true
			}
			list = append(list, at)
		}
	}
	if !found {
		return errorf("you (%s) are not on this event's guest list, so there is no invitation to respond to; if you organize it, use update_event", me), nil
	}
	su := a.SendUpdates
	if su == "" {
		su = "all"
	}
	return result(b.call(ctx, hc, http.MethodPatch, eventURL(a.calendar(), a.EventID, query{"sendUpdates": su}), object{"attendees": list}))
}
