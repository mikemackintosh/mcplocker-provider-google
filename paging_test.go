package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// pager is a Google list API three pages long: it answers by pageToken,
// honours the field mask the way Google does (nextPageToken is only sent
// when the mask names it), and records every URL it saw.
type pager struct {
	key   string
	urls  []string
	extra string // more top-level fields, e.g. resultSizeEstimate
}

func (p *pager) Do(r *http.Request) (*http.Response, error) {
	p.urls = append(p.urls, r.URL.String())
	page := map[string]int{"": 0, "p2": 1, "p3": 2}[r.URL.Query().Get("pageToken")]
	items := []string{}
	for i := 0; i < 2; i++ {
		items = append(items, fmt.Sprintf(`{"id":"%d-%d"}`, page, i))
	}
	body := fmt.Sprintf(`{"kind":"x","%s":[%s]`, p.key, strings.Join(items, ","))
	fields := r.URL.Query().Get("fields")
	if page < 2 && (fields == "" || strings.Contains(fields, "nextPageToken")) {
		body += fmt.Sprintf(`,"nextPageToken":"p%d"`, page+2)
	}
	body += p.extra + "}"
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// Every list tool Google can page, and the key its items come back under.
var listTools = []struct{ service, tool, args, key string }{
	{"drive", "search_files", `{"query":"name contains 'x'"}`, "files"},
	{"calendar", "list_calendars", `{}`, "items"},
	{"calendar", "list_events", `{"time_min":"2026-01-01T00:00:00Z","time_max":"2027-01-01T00:00:00Z"}`, "items"},
	{"gmail", "search_messages", `{"query":"is:unread"}`, "messages"},
	{"gmail", "list_drafts", `{}`, "drafts"},
	{"tasks", "list_task_lists", `{}`, "items"},
	{"tasks", "list_tasks", `{"task_list_id":"L1"}`, "items"},
}

func TestEveryListToolEnumeratesToTheEnd(t *testing.T) {
	for _, lt := range listTools {
		t.Run(lt.tool, func(t *testing.T) {
			p := &pager{key: lt.key}
			s := svc(t, p, lt.service)
			var ids []string
			token := ""
			for range 5 {
				var a map[string]any
				_ = json.Unmarshal([]byte(lt.args), &a)
				if token != "" {
					a["page_token"] = token
				}
				b, _ := json.Marshal(a)
				o := structured(t, run(t, s, lt.tool, string(b)))
				items, _ := o[lt.key].([]any)
				for _, it := range items {
					ids = append(ids, it.(map[string]any)["id"].(string))
				}
				token, _ = o["next_page_token"].(string)
				if token == "" {
					break
				}
			}
			if strings.Join(ids, ",") != "0-0,0-1,1-0,1-1,2-0,2-1" {
				t.Fatalf("enumerated %v via %v", ids, p.urls)
			}
			if len(p.urls) != 3 || !strings.Contains(p.urls[2], "pageToken=p3") {
				t.Fatalf("requests: %v", p.urls)
			}
			// The other arguments go with every page, as Google requires.
			if lt.tool == "list_events" && !strings.Contains(p.urls[2], "timeMin=") {
				t.Fatalf("page 3 dropped the window: %s", p.urls[2])
			}
			// Every description tells the model how to page.
			tools, _ := s.Tools(t.Context(), nil)
			for _, tl := range tools {
				if tl.Name == lt.tool && (!strings.Contains(tl.Description, "page_token") || !strings.Contains(string(tl.InputSchema), `"page_token"`)) {
					t.Fatalf("%s does not say how to page", lt.tool)
				}
			}
		})
	}
}

func TestPageSizesGoUpToGooglesMaximumAndNoFurther(t *testing.T) {
	for _, c := range []struct{ service, tool, args, want string }{
		{"gmail", "search_messages", `{"query":"x","max_results":5000}`, "maxResults=500"},
		{"drive", "search_files", `{"query":"x","page_size":1000}`, "pageSize=1000"},
		{"calendar", "list_events", `{"time_min":"a","time_max":"b","max_results":2500}`, "maxResults=2500"},
		{"tasks", "list_tasks", `{"max_results":900}`, "maxResults=100"},
	} {
		p := &pager{key: "items"}
		run(t, svc(t, p, c.service), c.tool, c.args)
		if !strings.Contains(p.urls[0], c.want) {
			t.Fatalf("%s: %s lacks %s", c.tool, p.urls[0], c.want)
		}
	}
}

func TestGmailSearchSaysWhenNothingMatchedAndHowManyItGuesses(t *testing.T) {
	// Gmail leaves "messages" out entirely when nothing matches.
	f := &fakeHTTP{status: 200, body: `{"resultSizeEstimate":0}`}
	o := structured(t, run(t, svc(t, f, "gmail"), "search_messages", `{"query":"from:nobody"}`))
	if msgs, ok := o["messages"].([]any); !ok || len(msgs) != 0 || o["result_size_estimate"] != float64(0) {
		t.Fatalf("no matches: %v", o)
	}
	p := &pager{key: "messages", extra: `,"resultSizeEstimate":4812`}
	o = structured(t, run(t, svc(t, p, "gmail"), "search_messages", `{"label_ids":["INBOX","UNREAD"],"include_spam_trash":true}`))
	if o["result_size_estimate"] != float64(4812) {
		t.Fatalf("estimate: %v", o)
	}
	// Label ids repeat as Gmail expects; an empty query lists everything.
	if u := p.urls[0]; !strings.Contains(u, "labelIds=INBOX&labelIds=UNREAD") || !strings.Contains(u, "includeSpamTrash=true") || strings.Contains(u, "q=") {
		t.Fatalf("url: %s", u)
	}
}

func TestDriveListsEverythingAndSharedDrives(t *testing.T) {
	p := &pager{key: "files"}
	run(t, svc(t, p, "drive"), "search_files", `{"include_shared_drives":true,"order_by":"modifiedTime desc"}`)
	u := p.urls[0]
	for _, want := range []string{"q=trashed+%3D+false", "corpora=allDrives", "includeItemsFromAllDrives=true", "supportsAllDrives=true", "orderBy=modifiedTime+desc", "fields=nextPageToken%2C"} {
		if !strings.Contains(u, want) {
			t.Fatalf("%s lacks %s", u, want)
		}
	}
}

func TestGetLabelCountsExactly(t *testing.T) {
	s := &scripted{routes: map[string]string{
		"GET /gmail/v1/users/me/labels":         `{"labels":[{"id":"INBOX","name":"INBOX"},{"id":"Label_7","name":"Receipts"}]}`,
		"GET /gmail/v1/users/me/labels/Label_7": `{"id":"Label_7","name":"Receipts","messagesTotal":5023,"messagesUnread":4411,"threadsTotal":3000,"threadsUnread":2900}`,
	}}
	o := structured(t, run(t, svc(t, s, "gmail"), "get_label", `{"label":"receipts"}`))
	if o["messagesUnread"] != float64(4411) || o["messagesTotal"] != float64(5023) {
		t.Fatalf("counts: %v", o)
	}
	res := run(t, svc(t, s, "gmail"), "get_label", `{"label":"No such label"}`)
	if !res.IsError {
		t.Fatalf("an unknown label should say so: %+v", res)
	}
}
