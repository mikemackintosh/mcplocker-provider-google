package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mcplocker/provider-google/internal/provider"
)

const tasksBase = apiBase + "/tasks/v1"

// What the Tasks API leaves to the caller, done here:
//
//   - A due date is a date. The field is RFC 3339 but the time part is
//     discarded, so the tools take YYYY-MM-DD (or a timestamp, keeping the
//     day) and send midnight UTC, which is what the Tasks UI writes.
//   - Completed tasks are hidden from list calls unless showHidden is set;
//     asking to see completed tasks sets both flags.
//   - Completing is status=completed; the API stamps completed. Reopening
//     is status=needsAction with completed cleared.
//   - "@default" is your default list.

var tasksTools = []provider.Tool{
	{Name: "list_task_lists", Title: "List task lists",
		Description: "Your task lists, with id and title. '@default' is always your default list." + pagingNote,
		InputSchema: schema(`{"type":"object","properties":{
			"max_results":{"type":"integer","minimum":1,"maximum":100,"default":100},
			"page_token":{"type":"string"}}}`),
		Annotations: readOnly},
	{Name: "create_task_list", Title: "Create a task list",
		InputSchema: schema(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`),
		Annotations: writes},
	{Name: "delete_task_list", Title: "Delete a task list",
		Description: "Delete a list and every task in it. Cannot be undone.",
		InputSchema: schema(`{"type":"object","properties":{"task_list_id":{"type":"string"}},"required":["task_list_id"]}`),
		Annotations: destructive},
	{Name: "list_tasks", Title: "List tasks",
		Description: "Tasks in a list. Open tasks by default; show_completed includes completed ones (which the API otherwise hides). Filter by due or completion date. Ordered as in the UI." + pagingNote,
		InputSchema: schema(`{"type":"object","properties":{
			"task_list_id":{"type":"string","default":"@default"},
			"show_completed":{"type":"boolean","default":false},
			"due_min":{"type":"string","description":"YYYY-MM-DD or RFC 3339, inclusive."},
			"due_max":{"type":"string"},
			"completed_min":{"type":"string"},
			"completed_max":{"type":"string"},
			"max_results":{"type":"integer","minimum":1,"maximum":100,"default":100},
			"page_token":{"type":"string"}}}`),
		Annotations: readOnly},
	{Name: "get_task", Title: "Get a task",
		InputSchema: schema(`{"type":"object","properties":{
			"task_list_id":{"type":"string","default":"@default"},
			"task_id":{"type":"string"}},"required":["task_id"]}`),
		Annotations: readOnly},
	{Name: "create_task", Title: "Record a task",
		Description: "Create a task. due is a date (YYYY-MM-DD); a time, if given, is discarded by the API. parent makes it a subtask; previous places it after a sibling (omit for the top).",
		InputSchema: schema(`{"type":"object","properties":{
			"task_list_id":{"type":"string","default":"@default"},
			"title":{"type":"string"},
			"notes":{"type":"string"},
			"due":{"type":"string","description":"YYYY-MM-DD"},
			"parent":{"type":"string","description":"Parent task id, for a subtask."},
			"previous":{"type":"string","description":"Sibling task id to place this after."}},
			"required":["title"]}`),
		Annotations: writes},
	{Name: "update_task", Title: "Update a task",
		Description: "Change a task's title, notes, due date or status; only what you pass changes. clear_due removes the date.",
		InputSchema: schema(`{"type":"object","properties":{
			"task_list_id":{"type":"string","default":"@default"},
			"task_id":{"type":"string"},
			"title":{"type":"string"},
			"notes":{"type":"string"},
			"due":{"type":"string","description":"YYYY-MM-DD"},
			"clear_due":{"type":"boolean"},
			"status":{"type":"string","enum":["needsAction","completed"]}},
			"required":["task_id"]}`),
		Annotations: idempotentW},
	{Name: "complete_task", Title: "Complete a task",
		Description: "Mark a task done. The API records when.",
		InputSchema: schema(`{"type":"object","properties":{
			"task_list_id":{"type":"string","default":"@default"},
			"task_id":{"type":"string"}},"required":["task_id"]}`),
		Annotations: idempotentW},
	{Name: "reopen_task", Title: "Reopen a task",
		InputSchema: schema(`{"type":"object","properties":{
			"task_list_id":{"type":"string","default":"@default"},
			"task_id":{"type":"string"}},"required":["task_id"]}`),
		Annotations: idempotentW},
	{Name: "move_task", Title: "Move a task",
		Description: "Reorder or re-parent a task within its list: parent for the new parent (omit for top level), previous for the sibling to follow (omit for first).",
		InputSchema: schema(`{"type":"object","properties":{
			"task_list_id":{"type":"string","default":"@default"},
			"task_id":{"type":"string"},
			"parent":{"type":"string"},
			"previous":{"type":"string"}},"required":["task_id"]}`),
		Annotations: idempotentW},
	{Name: "delete_task", Title: "Delete a task",
		Description: "Delete a task. Cannot be undone; complete_task is the reversible way to be done with one.",
		InputSchema: schema(`{"type":"object","properties":{
			"task_list_id":{"type":"string","default":"@default"},
			"task_id":{"type":"string"}},"required":["task_id"]}`),
		Annotations: destructive},
	{Name: "clear_completed_tasks", Title: "Clear completed tasks",
		Description: "Hide every completed task in a list (what 'Clear completed' does in the UI). They remain retrievable with show_completed.",
		InputSchema: schema(`{"type":"object","properties":{"task_list_id":{"type":"string","default":"@default"}}}`),
		Annotations: idempotentW},
}

type taskArgs struct {
	TaskListID string  `json:"task_list_id"`
	TaskID     string  `json:"task_id"`
	Title      *string `json:"title"`
	Notes      *string `json:"notes"`
	Due        string  `json:"due"`
	ClearDue   bool    `json:"clear_due"`
	Status     string  `json:"status"`
	Parent     string  `json:"parent"`
	Previous   string  `json:"previous"`
}

func (a *taskArgs) list() string {
	if a.TaskListID == "" {
		return "@default"
	}
	return a.TaskListID
}

func taskURL(list, task string, q query) string {
	u := tasksBase + "/lists/" + pathEsc(list) + "/tasks"
	if task != "" {
		u += "/" + pathEsc(task)
	}
	if len(q) > 0 {
		u += "?" + q.encode()
	}
	return u
}

// dueDate is the API's due: the day at midnight UTC, whatever time was given.
func dueDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	if d, err := time.Parse("2006-01-02", s); err == nil {
		return d.UTC().Format("2006-01-02T00:00:00.000Z"), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format("2006-01-02") + "T00:00:00.000Z", nil
	}
	return "", fmt.Errorf("%q is not YYYY-MM-DD", s)
}

// dateBound is an RFC 3339 bound for the list filters; a date means its start.
func dateBound(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if d, err := time.Parse("2006-01-02", s); err == nil {
		return d.UTC().Format(time.RFC3339), nil
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return s, nil
	}
	return "", fmt.Errorf("%q is not YYYY-MM-DD or RFC 3339", s)
}

func (b *Bundle) callTasks(ctx context.Context, hc Doer, req provider.CallRequest) (provider.CallResult, error) {
	var a taskArgs
	if err := args(req, &a); err != nil {
		return errResult(err), nil
	}
	switch req.Tool {
	case "list_task_lists":
		var p struct {
			MaxResults int `json:"max_results"`
			pageArgs
		}
		if err := args(req, &p); err != nil {
			return errResult(err), nil
		}
		if p.MaxResults == 0 {
			p.MaxResults = 100
		}
		return b.get(ctx, hc, tasksBase+"/users/@me/lists?"+paged(query{}, "maxResults", p.MaxResults, 100, p.PageToken).encode(), "items")
	case "create_task_list":
		if a.Title == nil || strings.TrimSpace(*a.Title) == "" {
			return errorf("title is required"), nil
		}
		return result(b.call(ctx, hc, http.MethodPost, tasksBase+"/users/@me/lists", object{"title": *a.Title}))
	case "delete_task_list":
		if a.TaskListID == "" || a.TaskListID == "@default" {
			return errorf("task_list_id is required and the default list cannot be deleted"), nil
		}
		if _, err := b.call(ctx, hc, http.MethodDelete, tasksBase+"/users/@me/lists/"+pathEsc(a.TaskListID), nil); err != nil {
			return result(nil, err)
		}
		return objResult(object{"deleted": true, "task_list_id": a.TaskListID}), nil
	case "list_tasks":
		var f struct {
			ShowCompleted bool   `json:"show_completed"`
			DueMin        string `json:"due_min"`
			DueMax        string `json:"due_max"`
			CompletedMin  string `json:"completed_min"`
			CompletedMax  string `json:"completed_max"`
			MaxResults    int    `json:"max_results"`
			PageToken     string `json:"page_token"`
		}
		if err := args(req, &f); err != nil {
			return errResult(err), nil
		}
		q := paged(query{"showCompleted": fmt.Sprint(f.ShowCompleted), "showHidden": fmt.Sprint(f.ShowCompleted)}, "maxResults", f.MaxResults, 100, f.PageToken)
		for k, v := range map[string]string{"dueMin": f.DueMin, "dueMax": f.DueMax, "completedMin": f.CompletedMin, "completedMax": f.CompletedMax} {
			bound, err := dateBound(v)
			if err != nil {
				return errorf("%s: %v", k, err), nil
			}
			if bound != "" {
				q[k] = bound
			}
		}
		return b.get(ctx, hc, taskURL(a.list(), "", q), "items")
	case "get_task":
		if a.TaskID == "" {
			return errorf("task_id is required"), nil
		}
		return result(b.call(ctx, hc, http.MethodGet, taskURL(a.list(), a.TaskID, nil), nil))
	case "create_task":
		if a.Title == nil || strings.TrimSpace(*a.Title) == "" {
			return errorf("title is required"), nil
		}
		body := object{"title": *a.Title}
		if a.Notes != nil {
			body["notes"] = *a.Notes
		}
		if a.Due != "" {
			d, err := dueDate(a.Due)
			if err != nil {
				return errorf("due: %v", err), nil
			}
			body["due"] = d
		}
		q := query{}
		if a.Parent != "" {
			q["parent"] = a.Parent
		}
		if a.Previous != "" {
			q["previous"] = a.Previous
		}
		return result(b.call(ctx, hc, http.MethodPost, taskURL(a.list(), "", q), body))
	case "update_task":
		if a.TaskID == "" {
			return errorf("task_id is required"), nil
		}
		patch := object{}
		if a.Title != nil {
			patch["title"] = *a.Title
		}
		if a.Notes != nil {
			patch["notes"] = *a.Notes
		}
		switch {
		case a.ClearDue:
			patch["due"] = nil
		case a.Due != "":
			d, err := dueDate(a.Due)
			if err != nil {
				return errorf("due: %v", err), nil
			}
			patch["due"] = d
		}
		switch a.Status {
		case "":
		case "completed":
			patch["status"] = "completed"
		case "needsAction":
			patch["status"], patch["completed"] = "needsAction", nil
		default:
			return errorf("status must be needsAction or completed"), nil
		}
		if len(patch) == 0 {
			return errorf("nothing to change"), nil
		}
		return result(b.call(ctx, hc, http.MethodPatch, taskURL(a.list(), a.TaskID, nil), patch))
	case "complete_task":
		if a.TaskID == "" {
			return errorf("task_id is required"), nil
		}
		return result(b.call(ctx, hc, http.MethodPatch, taskURL(a.list(), a.TaskID, nil), object{"status": "completed"}))
	case "reopen_task":
		if a.TaskID == "" {
			return errorf("task_id is required"), nil
		}
		return result(b.call(ctx, hc, http.MethodPatch, taskURL(a.list(), a.TaskID, nil), object{"status": "needsAction", "completed": nil}))
	case "move_task":
		if a.TaskID == "" {
			return errorf("task_id is required"), nil
		}
		q := query{}
		if a.Parent != "" {
			q["parent"] = a.Parent
		}
		if a.Previous != "" {
			q["previous"] = a.Previous
		}
		u := taskURL(a.list(), a.TaskID, nil) + "/move"
		if len(q) > 0 {
			u += "?" + q.encode()
		}
		return result(b.call(ctx, hc, http.MethodPost, u, nil))
	case "delete_task":
		if a.TaskID == "" {
			return errorf("task_id is required"), nil
		}
		if _, err := b.call(ctx, hc, http.MethodDelete, taskURL(a.list(), a.TaskID, nil), nil); err != nil {
			return result(nil, err)
		}
		return objResult(object{"deleted": true, "task_id": a.TaskID, "task_list_id": a.list()}), nil
	case "clear_completed_tasks":
		if _, err := b.call(ctx, hc, http.MethodPost, tasksBase+"/lists/"+pathEsc(a.list())+"/clear", nil); err != nil {
			return result(nil, err)
		}
		return objResult(object{"cleared": true, "task_list_id": a.list()}), nil
	}
	return provider.CallResult{}, fmt.Errorf("google/tasks: unknown tool %q", req.Tool)
}
