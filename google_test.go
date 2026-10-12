package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/mikemackintosh/mcplocker-provider-google/internal/ifc"
	"github.com/mikemackintosh/mcplocker-provider-google/internal/provider"
)

// fakeHTTP answers one canned body to every request (the single-call tools).
type fakeHTTP struct {
	gotURL, gotAuth string
	status          int
	body            string
}

func (f *fakeHTTP) Do(r *http.Request) (*http.Response, error) {
	f.gotURL, f.gotAuth = r.URL.String(), r.Header.Get("Authorization")
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body)), Header: http.Header{}}, nil
}

// scripted answers by method and path prefix, and records every request,
// for the tools that make several calls.
type scripted struct {
	routes map[string]string // "GET /calendar/v3/calendars/primary" → body
	calls  []call
}

type call struct {
	Method, URL string
	Body        object
	Raw         string
}

func (s *scripted) Do(r *http.Request) (*http.Response, error) {
	c := call{Method: r.Method, URL: r.URL.String()}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		c.Raw = string(raw)
		_ = json.Unmarshal(raw, &c.Body)
	}
	s.calls = append(s.calls, c)
	// The longest matching prefix wins, so /calendars/primary does not
	// answer for /calendars/primary/events/ev1.
	best, bestBody, found := "", "", false
	for k, body := range s.routes {
		method, path, _ := strings.Cut(k, " ")
		path = strings.TrimSuffix(path, "?")
		if r.Method == method && strings.HasPrefix(r.URL.Path, path) && len(path) > len(best) {
			best, bestBody, found = path, body, true
		}
	}
	if found {
		status := 200
		if bestBody == "" {
			status = 204
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(bestBody)), Header: http.Header{}}, nil
	}
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"no route in test for ` + r.Method + " " + r.URL.Path + `"}}`)), Header: http.Header{}}, nil
}

func (s *scripted) find(method, pathPrefix string) *call {
	for i := range s.calls {
		if s.calls[i].Method == method && strings.Contains(s.calls[i].URL, pathPrefix) {
			return &s.calls[i]
		}
	}
	return nil
}

// bound is a service with the client its calls go out on, as one call in
// the image would have.
type bound struct {
	s *service
	d Doer
}

func (b *bound) Slug() string { return b.s.slug }

func (b *bound) Tools(context.Context, any) ([]provider.Tool, error) { return b.s.tools, nil }

func svc(t *testing.T, d Doer, slug string) *bound {
	t.Helper()
	for _, s := range services() {
		if s.slug == slug {
			return &bound{s: s, d: d}
		}
	}
	t.Fatalf("no service %s", slug)
	return nil
}

func run(t *testing.T, s *bound, tool, argsJSON string) provider.CallResult {
	t.Helper()
	res, err := s.s.call(context.Background(), s.d, provider.CallRequest{Tool: tool, Arguments: json.RawMessage(argsJSON)})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func structured(t *testing.T, res provider.CallResult) object {
	t.Helper()
	var o object
	if err := json.Unmarshal(res.StructuredContent, &o); err != nil {
		t.Fatalf("structuredContent is not an object: %s", res.StructuredContent)
	}
	return o
}

func TestToolsAndScopes(t *testing.T) {
	if len(services()) != 4 {
		t.Fatalf("%d services", len(services()))
	}
	for _, s := range services() {
		if s.slug == "gmail" && !hasLabel(s.labels, "pii") {
			t.Fatal("gmail must carry pii label")
		}
	}
	// Every tool has a valid schema; the annotations say what it does.
	writers := map[string]bool{}
	for _, s := range services() {
		for _, tool := range s.tools {
			var sc map[string]any
			if err := json.Unmarshal(tool.InputSchema, &sc); err != nil {
				t.Fatalf("%s/%s: bad schema: %v", s.slug, tool.Name, err)
			}
			if tool.Description == "" && tool.Title == "" {
				t.Fatalf("%s/%s: no description", s.slug, tool.Name)
			}
			ro := tool.Annotations.ReadOnly
			isRead := strings.HasPrefix(tool.Name, "list_") || strings.HasPrefix(tool.Name, "get_") || strings.HasPrefix(tool.Name, "search_") || strings.HasPrefix(tool.Name, "find_")
			if ro != isRead {
				t.Fatalf("%s/%s: readOnly=%v", s.slug, tool.Name, ro)
			}
			if strings.HasPrefix(tool.Name, "delete_") && !tool.Annotations.Destructive {
				t.Fatalf("%s/%s must be destructive", s.slug, tool.Name)
			}
			if strings.HasPrefix(tool.Name, "send_") && (!tool.Annotations.Destructive || !tool.Annotations.OpenWorld) {
				t.Fatalf("%s/%s: sending is destructive and open-world", s.slug, tool.Name)
			}
			if !ro {
				writers[s.slug] = true
			}
		}
	}
	if writers["drive"] {
		t.Fatal("drive stays read-only")
	}
	// The manifest is what the gateway reads: it asks for each service's
	// scopes, and its consent-screen step lists every one.
	m, err := os.ReadFile("mcplocker-provider.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range services() {
		for _, sc := range s.scopes {
			if strings.Count(string(m), sc) < 2 {
				t.Fatalf("the manifest does not ask for %s on %s and list it in setup", sc, s.slug)
			}
		}
		for _, tool := range s.tools {
			if !strings.Contains(string(m), "      "+tool.Name+":") {
				t.Fatalf("the manifest does not tag %s/%s", s.slug, tool.Name)
			}
		}
	}
}

func TestSearchFilesCall(t *testing.T) {
	f := &fakeHTTP{status: 200, body: `{"files":[{"id":"1","name":"roadmap"},{"id":"2","name":"roadmap v2"}]}`}
	res := run(t, svc(t, f, "drive"), "search_files", `{"query":"name contains 'roadmap'","page_size":5}`)
	if f.gotAuth != "" || !strings.HasPrefix(f.gotURL, "https://www.googleapis.com/drive/v3/files?") || !strings.Contains(f.gotURL, "pageSize=5") {
		t.Fatalf("url=%s auth=%s", f.gotURL, f.gotAuth)
	}
	o := structured(t, res)
	if files, _ := o["files"].([]any); len(files) != 2 {
		t.Fatalf("structuredContent should be {files: […]}: %s", res.StructuredContent)
	}
}

func TestListEventsIsAnObject(t *testing.T) {
	f := &fakeHTTP{status: 200, body: `{"kind":"calendar#events","items":[{"id":"a","summary":"standup"}]}`}
	res := run(t, svc(t, f, "calendar"), "list_events", `{"time_min":"2026-01-01T00:00:00Z","time_max":"2026-01-02T00:00:00Z"}`)
	o := structured(t, res)
	if items, _ := o["items"].([]any); len(items) != 1 {
		t.Fatalf("structuredContent: %s", res.StructuredContent)
	}
	if !strings.Contains(f.gotURL, "singleEvents=true") || !strings.Contains(f.gotURL, "/calendars/primary/events?") {
		t.Fatalf("url: %s", f.gotURL)
	}
}

func TestUpstreamErrorIsToolError(t *testing.T) {
	f := &fakeHTTP{status: 403, body: `{"error":{"message":"insufficient scope"}}`}
	res := run(t, svc(t, f, "calendar"), "list_events", `{"time_min":"2026-01-01T00:00:00Z","time_max":"2026-01-02T00:00:00Z"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "403") {
		t.Fatalf("%+v", res)
	}
}

// ---- calendar ------------------------------------------------------------------

func calendarFake() *scripted {
	return &scripted{routes: map[string]string{
		"GET /calendar/v3/calendars/primary":               `{"id":"me@example.com"}`,
		"GET /calendar/v3/users/me/settings/timezone":      `{"value":"America/New_York"}`,
		"POST /calendar/v3/calendars/primary/events":       `{"id":"ev1","htmlLink":"https://calendar.google.com/x","hangoutLink":"https://meet.google.com/abc"}`,
		"PATCH /calendar/v3/calendars/primary/events/ev1":  `{"id":"ev1"}`,
		"DELETE /calendar/v3/calendars/primary/events/ev1": "",
	}}
}

// TestCreateEventAddsSelfMeetAndInvites: the three things the raw API
// leaves out — you on the guest list, a Meet link that actually attaches,
// invitations that actually go — plus the zone for a bare local time.
func TestCreateEventAddsSelfMeetAndInvites(t *testing.T) {
	f := calendarFake()
	res := run(t, svc(t, f, "calendar"), "create_event", `{"summary":"Planning","start":"2026-10-01T10:00:00","end":"2026-10-01T10:30:00","attendees":["a@example.com"],"optional_attendees":["b@example.com"],"google_meet":true,"reminder_minutes":[10],"location":"Room 4"}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	post := f.find("POST", "/events?")
	if post == nil {
		t.Fatalf("no insert: %+v", f.calls)
	}
	if !strings.Contains(post.URL, "sendUpdates=all") || !strings.Contains(post.URL, "conferenceDataVersion=1") {
		t.Fatalf("query: %s", post.URL)
	}
	att, _ := post.Body["attendees"].([]any)
	if len(att) != 3 {
		t.Fatalf("attendees: %v", post.Body["attendees"])
	}
	self := att[2].(map[string]any)
	if self["email"] != "me@example.com" || self["responseStatus"] != "accepted" || self["self"] != true {
		t.Fatalf("self attendee: %v", self)
	}
	if opt := att[1].(map[string]any); opt["optional"] != true {
		t.Fatalf("optional attendee: %v", opt)
	}
	start := post.Body["start"].(map[string]any)
	if start["dateTime"] != "2026-10-01T10:00:00" || start["timeZone"] != "America/New_York" {
		t.Fatalf("start uses the calendar's zone: %v", start)
	}
	cd := post.Body["conferenceData"].(map[string]any)["createRequest"].(map[string]any)
	if cd["conferenceSolutionKey"].(map[string]any)["type"] != "hangoutsMeet" || cd["requestId"] == "" {
		t.Fatalf("meet request: %v", cd)
	}
	rem := post.Body["reminders"].(map[string]any)
	if rem["useDefault"] != false {
		t.Fatalf("reminders: %v", rem)
	}
	if structured(t, res)["hangoutLink"] != "https://meet.google.com/abc" {
		t.Fatal("result is the created event")
	}

	// No guests, no self: nothing emailed, no attendee list at all.
	f2 := calendarFake()
	run(t, svc(t, f2, "calendar"), "create_event", `{"summary":"Focus","start":"2026-10-02","add_self":false}`)
	post = f2.find("POST", "/events?")
	if !strings.Contains(post.URL, "sendUpdates=none") || post.Body["attendees"] != nil {
		t.Fatalf("solo event: %s %v", post.URL, post.Body)
	}
	// All-day: inclusive end in, exclusive end out; one day when end omitted.
	if post.Body["start"].(map[string]any)["date"] != "2026-10-02" || post.Body["end"].(map[string]any)["date"] != "2026-10-03" {
		t.Fatalf("all-day dates: %v %v", post.Body["start"], post.Body["end"])
	}
	// Validation is a tool error, before any call.
	f3 := calendarFake()
	res = run(t, svc(t, f3, "calendar"), "create_event", `{"summary":"x","start":"2026-10-01T10:00:00Z","end":"2026-10-01T09:00:00Z"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "end must be after start") || f3.find("POST", "/events") != nil {
		t.Fatalf("validation: %+v", res)
	}
	res = run(t, svc(t, f3, "calendar"), "create_event", `{"summary":"x","start":"2026-10-01T10:00:00Z","end":"2026-10-01T11:00:00Z","attendees":["not an email"]}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not an email address") {
		t.Fatalf("email validation: %+v", res)
	}
}

// TestUpdateEventMergesAttendees: PATCH replaces the list, so add/remove
// read it first and send the merged list.
func TestUpdateEventMergesAttendees(t *testing.T) {
	f := calendarFake()
	f.routes["GET /calendar/v3/calendars/primary/events/ev1"] = `{"attendees":[{"email":"a@example.com","responseStatus":"accepted"},{"email":"gone@example.com"},{"email":"me@example.com","self":true,"organizer":true}]}`
	res := run(t, svc(t, f, "calendar"), "update_event", `{"event_id":"ev1","add_attendees":["new@example.com"],"remove_attendees":["gone@example.com"],"summary":"Renamed"}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	patch := f.find("PATCH", "/events/ev1")
	if patch == nil || !strings.Contains(patch.URL, "sendUpdates=all") {
		t.Fatalf("patch: %+v", patch)
	}
	att := patch.Body["attendees"].([]any)
	emails := []string{}
	for _, x := range att {
		emails = append(emails, x.(map[string]any)["email"].(string))
	}
	if strings.Join(emails, ",") != "a@example.com,me@example.com,new@example.com" {
		t.Fatalf("merged: %v", emails)
	}
	if att[0].(map[string]any)["responseStatus"] != "accepted" {
		t.Fatal("existing responses kept")
	}
	if patch.Body["summary"] != "Renamed" {
		t.Fatal("other fields patched too")
	}
	// A rename alone emails nobody.
	f2 := calendarFake()
	run(t, svc(t, f2, "calendar"), "update_event", `{"event_id":"ev1","summary":"Quiet"}`)
	if p := f2.find("PATCH", "/events/ev1"); !strings.Contains(p.URL, "sendUpdates=none") {
		t.Fatalf("quiet rename: %s", p.URL)
	}
}

func TestRespondToEvent(t *testing.T) {
	f := calendarFake()
	f.routes["GET /calendar/v3/calendars/primary/events/ev1"] = `{"attendees":[{"email":"org@example.com","organizer":true},{"email":"me@example.com","self":true,"responseStatus":"needsAction"}]}`
	res := run(t, svc(t, f, "calendar"), "respond_to_event", `{"event_id":"ev1","response":"tentative","comment":"may run late"}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	patch := f.find("PATCH", "/events/ev1")
	att := patch.Body["attendees"].([]any)
	me := att[1].(map[string]any)
	if me["responseStatus"] != "tentative" || me["comment"] != "may run late" || len(att) != 2 {
		t.Fatalf("response: %v", att)
	}
	// Not a guest: a clear tool error, no PATCH.
	f2 := calendarFake()
	f2.routes["GET /calendar/v3/calendars/primary/events/ev1"] = `{"attendees":[{"email":"other@example.com"}]}`
	res = run(t, svc(t, f2, "calendar"), "respond_to_event", `{"event_id":"ev1","response":"accepted"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not on this event's guest list") || f2.find("PATCH", "/events/ev1") != nil {
		t.Fatalf("not a guest: %+v", res)
	}
}

func TestDeleteEventSendsCancellation(t *testing.T) {
	f := calendarFake()
	res := run(t, svc(t, f, "calendar"), "delete_event", `{"event_id":"ev1"}`)
	del := f.find("DELETE", "/events/ev1")
	if del == nil || !strings.Contains(del.URL, "sendUpdates=all") || structured(t, res)["deleted"] != true {
		t.Fatalf("delete: %+v %s", del, res.StructuredContent)
	}
}

// ---- tasks ---------------------------------------------------------------------

func TestTasksDueDateAndCompletion(t *testing.T) {
	f := &scripted{routes: map[string]string{
		"POST /tasks/v1/lists/@default/tasks":         `{"id":"t1","title":"Send deck","due":"2026-10-03T00:00:00.000Z"}`,
		"PATCH /tasks/v1/lists/@default/tasks/t1":     `{"id":"t1","status":"completed"}`,
		"GET /tasks/v1/lists/@default/tasks":          `{"items":[{"id":"t1"}]}`,
		"DELETE /tasks/v1/lists/@default/tasks/t1":    "",
		"POST /tasks/v1/lists/@default/tasks/t1/move": `{"id":"t1","parent":"p1"}`,
	}}
	s := svc(t, f, "tasks")
	res := run(t, s, "create_task", `{"title":"Send deck","notes":"before Friday","due":"2026-10-03T15:30:00-07:00","parent":"p1"}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	post := f.find("POST", "/tasks?")
	if post.Body["due"] != "2026-10-03T00:00:00.000Z" || post.Body["notes"] != "before Friday" || !strings.Contains(post.URL, "parent=p1") {
		t.Fatalf("create: %s %v", post.URL, post.Body)
	}
	run(t, s, "complete_task", `{"task_id":"t1"}`)
	if p := f.find("PATCH", "/tasks/t1"); p.Body["status"] != "completed" {
		t.Fatalf("complete: %v", p.Body)
	}
	f.calls = nil
	run(t, s, "reopen_task", `{"task_id":"t1"}`)
	if p := f.find("PATCH", "/tasks/t1"); p.Body["status"] != "needsAction" || !strings.Contains(p.Raw, `"completed":null`) {
		t.Fatalf("reopen clears completed: %s", p.Raw)
	}
	f.calls = nil
	run(t, s, "list_tasks", `{"show_completed":true,"due_min":"2026-10-01"}`)
	if g := f.find("GET", "/tasks?"); !strings.Contains(g.URL, "showCompleted=true") || !strings.Contains(g.URL, "showHidden=true") || !strings.Contains(g.URL, "dueMin=2026-10-01T00%3A00%3A00Z") {
		t.Fatalf("list: %s", g.URL)
	}
	res = run(t, s, "create_task", `{"title":"x","due":"next tuesday"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "YYYY-MM-DD") {
		t.Fatalf("due validation: %+v", res)
	}
	res = run(t, s, "delete_task_list", `{"task_list_id":"@default"}`)
	if !res.IsError {
		t.Fatal("the default list cannot be deleted")
	}
	run(t, s, "move_task", `{"task_id":"t1","parent":"p1"}`)
	if m := f.find("POST", "/tasks/t1/move"); m == nil || !strings.Contains(m.URL, "parent=p1") {
		t.Fatalf("move: %+v", m)
	}
}

// ---- gmail ---------------------------------------------------------------------

const labelsBody = `{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"UNREAD","name":"UNREAD","type":"system"},{"id":"Label_7","name":"Receipts","type":"user"},{"id":"Label_9","name":"Projects/Apollo","type":"user"}]}`

func gmailFake() *scripted {
	return &scripted{routes: map[string]string{
		"GET /gmail/v1/users/me/labels":                       labelsBody,
		"POST /gmail/v1/users/me/labels":                      `{"id":"Label_new","name":"Vendors"}`,
		"GET /gmail/v1/users/me/profile":                      `{"emailAddress":"me@example.com"}`,
		"GET /gmail/v1/users/me/settings/forwardingAddresses": `{"forwardingAddresses":[{"forwardingEmail":"ok@example.org","verificationStatus":"accepted"},{"forwardingEmail":"pending@example.org","verificationStatus":"pending"}]}`,
		"POST /gmail/v1/users/me/settings/filters":            `{"id":"F2","criteria":{"from":"billing@vendor.com"},"action":{"addLabelIds":["Label_7"],"removeLabelIds":["INBOX"]}}`,
		"GET /gmail/v1/users/me/settings/filters/F1":          `{"id":"F1","criteria":{"from":"billing@vendor.com","hasAttachment":true},"action":{"addLabelIds":["Label_7"]}}`,
		"DELETE /gmail/v1/users/me/settings/filters/F1":       "",
		"GET /gmail/v1/users/me/settings/filters":             `{"filter":[{"id":"F1","criteria":{"from":"billing@vendor.com"},"action":{"addLabelIds":["Label_7"],"removeLabelIds":["INBOX"]}}]}`,
		"GET /gmail/v1/users/me/messages?":                    "",
		"POST /gmail/v1/users/me/messages/batchModify":        "",
		"POST /gmail/v1/users/me/drafts":                      `{"id":"d1","message":{"id":"m9","threadId":"th1"}}`,
		"POST /gmail/v1/users/me/messages/send":               `{"id":"m10","threadId":"th1"}`,
	}}
}

// TestCreateFilterResolvesLabelsAndArchives: names in, ids out; a missing
// user label is created; archive is "remove INBOX"; the result names the
// labels back.
func TestCreateFilterResolvesLabelsAndArchives(t *testing.T) {
	f := gmailFake()
	res := run(t, svc(t, f, "gmail"), "create_filter", `{"from":"billing@vendor.com","add_labels":["receipts","Vendors"],"archive":true,"mark_read":true}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	if c := f.find("POST", "/labels"); c == nil || c.Body["name"] != "Vendors" {
		t.Fatalf("missing label created: %+v", c)
	}
	post := f.find("POST", "/settings/filters")
	action := post.Body["action"].(map[string]any)
	if got := toStrings(action["addLabelIds"]); strings.Join(got, ",") != "Label_7,Label_new" {
		t.Fatalf("addLabelIds: %v", got)
	}
	if got := toStrings(action["removeLabelIds"]); strings.Join(got, ",") != "INBOX,UNREAD" {
		t.Fatalf("removeLabelIds: %v", got)
	}
	if post.Body["criteria"].(map[string]any)["from"] != "billing@vendor.com" {
		t.Fatalf("criteria: %v", post.Body["criteria"])
	}
	o := structured(t, res)
	if names := toStrings(o["add_label_names"]); strings.Join(names, ",") != "Receipts,Vendors" {
		t.Fatalf("names back: %v", names)
	}
	if f.find("GET", "/messages?") != nil {
		t.Fatal("existing mail untouched unless asked")
	}
	// No criterion / no action are tool errors before any call.
	f2 := gmailFake()
	res = run(t, svc(t, f2, "gmail"), "create_filter", `{"add_labels":["Receipts"]}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "at least one criterion") {
		t.Fatalf("no criterion: %+v", res)
	}
	res = run(t, svc(t, f2, "gmail"), "create_filter", `{"from":"x@y.com"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "at least one action") {
		t.Fatalf("no action: %+v", res)
	}
	// An unknown label without create is named in the error.
	res = run(t, svc(t, f2, "gmail"), "create_filter", `{"from":"x@y.com","add_labels":["Nope"],"create_missing_labels":false}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, `no label named "Nope"`) {
		t.Fatalf("unknown label: %+v", res)
	}
}

func TestCreateFilterForwardingMustBeVerified(t *testing.T) {
	f := gmailFake()
	res := run(t, svc(t, f, "gmail"), "create_filter", `{"from":"alerts@x.com","forward":"pending@example.org"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not verified") || f.find("POST", "/settings/filters") != nil {
		t.Fatalf("pending forward: %+v", res)
	}
	res = run(t, svc(t, f, "gmail"), "create_filter", `{"from":"alerts@x.com","forward":"nobody@example.org"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not a verified forwarding address") {
		t.Fatalf("unknown forward: %+v", res)
	}
	res = run(t, svc(t, f, "gmail"), "create_filter", `{"from":"alerts@x.com","forward":"ok@example.org"}`)
	if res.IsError {
		t.Fatalf("verified forward: %s", res.Content[0].Text)
	}
	if p := f.find("POST", "/settings/filters"); p.Body["action"].(map[string]any)["forward"] != "ok@example.org" {
		t.Fatalf("forward action: %v", p.Body)
	}
}

// TestCreateFilterAppliesToExisting: the search mirrors the criteria and
// the same label change is batched onto what it finds, across pages.
func TestCreateFilterAppliesToExisting(t *testing.T) {
	f := gmailFake()
	pages := []string{`{"messages":[{"id":"m1"},{"id":"m2"}],"nextPageToken":"p2"}`, `{"messages":[{"id":"m3"}]}`}
	f.routes["GET /gmail/v1/users/me/messages"] = "" // replaced by the pager below
	pager := &pagingDoer{inner: f, pages: pages}
	res := run(t, svc(t, pager, "gmail"), "create_filter", `{"from":"billing@vendor.com","has_attachment":true,"size_larger_than_bytes":1000000,"add_labels":["Receipts"],"archive":true,"apply_to_existing":true}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	search := f.find("GET", "/messages?")
	if search == nil || !strings.Contains(search.URL, "q=from%3A%28billing%40vendor.com%29+has%3Aattachment+larger%3A1000000") {
		t.Fatalf("search mirrors criteria: %+v", search)
	}
	var mods []call
	for _, c := range f.calls {
		if c.Method == "POST" && strings.Contains(c.URL, "batchModify") {
			mods = append(mods, c)
		}
	}
	if len(mods) != 2 || len(mods[0].Body["ids"].([]any)) != 2 || len(mods[1].Body["ids"].([]any)) != 1 {
		t.Fatalf("batches: %+v", mods)
	}
	if got := toStrings(mods[0].Body["removeLabelIds"]); strings.Join(got, ",") != "INBOX" {
		t.Fatalf("batch labels: %v", mods[0].Body)
	}
	if n, _ := structured(t, res)["applied_to_existing_messages"].(float64); n != 3 {
		t.Fatalf("count: %v", n)
	}
}

// pagingDoer serves successive bodies for the message search and defers
// everything else.
type pagingDoer struct {
	inner *scripted
	pages []string
	i     int
}

func (p *pagingDoer) Do(r *http.Request) (*http.Response, error) {
	if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/messages") {
		p.inner.calls = append(p.inner.calls, call{Method: r.Method, URL: r.URL.String()})
		body := `{"messages":[]}`
		if p.i < len(p.pages) {
			body = p.pages[p.i]
			p.i++
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	}
	return p.inner.Do(r)
}

// TestUpdateFilterReplaces: create the new one first, then delete the old;
// unchanged criteria carry over, a cleared one goes.
func TestUpdateFilterReplaces(t *testing.T) {
	f := gmailFake()
	res := run(t, svc(t, f, "gmail"), "update_filter", `{"filter_id":"F1","archive":true,"clear_criteria":["has_attachment"]}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	post := f.find("POST", "/settings/filters")
	crit := post.Body["criteria"].(map[string]any)
	if crit["from"] != "billing@vendor.com" || crit["hasAttachment"] != nil {
		t.Fatalf("criteria carried and cleared: %v", crit)
	}
	action := post.Body["action"].(map[string]any)
	if strings.Join(toStrings(action["addLabelIds"]), ",") != "Label_7" || strings.Join(toStrings(action["removeLabelIds"]), ",") != "INBOX" {
		t.Fatalf("action: %v", action)
	}
	var order []string
	for _, c := range f.calls {
		if strings.Contains(c.URL, "/settings/filters") {
			order = append(order, c.Method)
		}
	}
	if strings.Join(order, ",") != "GET,POST,DELETE" {
		t.Fatalf("create before delete: %v", order)
	}
	o := structured(t, res)
	if o["replaced_filter_id"] != "F1" || o["filter"].(map[string]any)["id"] != "F2" {
		t.Fatalf("result: %v", o)
	}
}

func TestListFiltersNamesLabels(t *testing.T) {
	res := run(t, svc(t, gmailFake(), "gmail"), "list_filters", `{}`)
	filters := structured(t, res)["filters"].([]any)
	action := filters[0].(map[string]any)["action"].(map[string]any)
	if strings.Join(toStrings(action["add_label_names"]), ",") != "Receipts" || strings.Join(toStrings(action["remove_label_names"]), ",") != "INBOX" {
		t.Fatalf("names: %v", action)
	}
}

// TestDraftReplyThreadsAndAddresses: the draft joins the thread with the
// original's Message-ID in In-Reply-To and References, Re: on the subject,
// reply-all recipients computed without you, and a real MIME body.
func TestDraftReplyThreadsAndAddresses(t *testing.T) {
	f := gmailFake()
	f.routes["GET /gmail/v1/users/me/messages/m1"] = `{"id":"m1","threadId":"th1","payload":{"headers":[
		{"name":"Message-ID","value":"<orig@vendor.com>"},{"name":"References","value":"<first@vendor.com>"},
		{"name":"Subject","value":"Invoice 42"},{"name":"From","value":"Ann <ann@vendor.com>"},
		{"name":"To","value":"me@example.com, Bob <bob@example.com>"},{"name":"Cc","value":"carol@example.com"}]}}`
	res := run(t, svc(t, f, "gmail"), "create_draft", `{"reply_to_message_id":"m1","reply_all":true,"body_text":"Paid today — thanks!","body_html":"<p>Paid today — thanks!</p>"}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	post := f.find("POST", "/drafts")
	msg := post.Body["message"].(map[string]any)
	if msg["threadId"] != "th1" {
		t.Fatalf("threadId: %v", msg)
	}
	rawB, err := base64.URLEncoding.DecodeString(msg["raw"].(string))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(rawB)
	for _, want := range []string{
		"From: me@example.com\r\n",
		"To: \"Ann\" <ann@vendor.com>, \"Bob\" <bob@example.com>\r\n",
		"Cc: <carol@example.com>\r\n",
		"Subject: Re: Invoice 42\r\n",
		"In-Reply-To: <orig@vendor.com>\r\n",
		"References: <first@vendor.com> <orig@vendor.com>\r\n",
		"Content-Type: multipart/alternative; boundary=",
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n",
		"Content-Type: text/html; charset=utf-8\r\n",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "To: me@example.com") || strings.Contains(strings.ToLower(raw), "cc: me@") {
		t.Fatal("never reply to yourself")
	}
	// The text part decodes to what was written, with its UTF-8 intact.
	enc := base64.StdEncoding.EncodeToString([]byte("Paid today — thanks!"))
	if !strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), enc) {
		t.Fatalf("body not base64 of the text:\n%s", raw)
	}
	// A non-ASCII subject is RFC 2047-encoded.
	f2 := gmailFake()
	run(t, svc(t, f2, "gmail"), "create_draft", `{"to":["Zoë <zoe@example.com>"],"subject":"Résumé","body_text":"hi"}`)
	rawB, _ = base64.URLEncoding.DecodeString(f2.find("POST", "/drafts").Body["message"].(map[string]any)["raw"].(string))
	if !strings.Contains(string(rawB), "Subject: =?utf-8?q?R=C3=A9sum=C3=A9?=\r\n") || !strings.Contains(string(rawB), `To: "Zoë" <zoe@example.com>`) && !strings.Contains(string(rawB), "To: =?utf-8?q?Zo=C3=AB?= <zoe@example.com>") {
		t.Fatalf("encoding:\n%s", rawB)
	}
	// Nothing to send to is a tool error before any call.
	f3 := gmailFake()
	res = run(t, svc(t, f3, "gmail"), "create_draft", `{"subject":"x","body_text":"y"}`)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "no recipients") || f3.find("POST", "/drafts") != nil {
		t.Fatalf("no recipients: %+v", res)
	}
	// send_message uses the same composition and the send endpoint.
	f4 := gmailFake()
	run(t, svc(t, f4, "gmail"), "send_message", `{"to":["a@example.com"],"subject":"s","body_text":"b"}`)
	if f4.find("POST", "/messages/send") == nil || f4.find("POST", "/drafts") != nil {
		t.Fatalf("send: %+v", f4.calls)
	}
}

func TestModifyMessageLabelsByName(t *testing.T) {
	f := gmailFake()
	res := run(t, svc(t, f, "gmail"), "modify_message_labels", `{"message_ids":["m1","m2"],"add_labels":["Receipts"],"remove_labels":["inbox","unread"]}`)
	if res.IsError {
		t.Fatalf("error: %s", res.Content[0].Text)
	}
	bm := f.find("POST", "/batchModify")
	if strings.Join(toStrings(bm.Body["addLabelIds"]), ",") != "Label_7" || strings.Join(toStrings(bm.Body["removeLabelIds"]), ",") != "INBOX,UNREAD" {
		t.Fatalf("batchModify: %v", bm.Body)
	}
	// Removing a label that does not exist is never "create it".
	res = run(t, svc(t, gmailFake(), "gmail"), "modify_message_labels", `{"message_ids":["m1"],"remove_labels":["Nope"]}`)
	if !res.IsError {
		t.Fatal("unknown label to remove is an error")
	}
}

func toStrings(v any) []string {
	out := []string{}
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			out = append(out, x.(string))
		}
	}
	return out
}

func hasLabel(ls []ifc.Label, want ifc.Label) bool {
	for _, l := range ls {
		if l == want {
			return true
		}
	}
	return false
}
