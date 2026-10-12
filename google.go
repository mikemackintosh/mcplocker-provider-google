package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/mikemackintosh/mcplocker-provider-google/internal/ifc"
	"github.com/mikemackintosh/mcplocker-provider-google/internal/provider"
)

// The write tools carry the knowledge the raw APIs leave to the caller: a
// created meeting has to list its creator as an attendee or it shows no
// response state; invitations only go out with sendUpdates; a Google Meet
// link needs conferenceDataVersion=1; a Gmail filter cannot be edited, only
// replaced, and never touches mail that already arrived; a reply threads
// only with In-Reply-To and References copied from the original. Each tool
// does those things, and its description says so, so an agent does not
// have to know.

const (
	apiBase   = "https://www.googleapis.com"
	gmailBase = "https://gmail.googleapis.com"
)

// The scopes each service asks for. The manifest is what the gateway reads;
// these are here so a test can hold the two together.
const (
	scopeCalendar      = "https://www.googleapis.com/auth/calendar"
	scopeTasks         = "https://www.googleapis.com/auth/tasks"
	scopeDriveRO       = "https://www.googleapis.com/auth/drive.readonly"
	scopeGmailModify   = "https://www.googleapis.com/auth/gmail.modify"
	scopeGmailSettings = "https://www.googleapis.com/auth/gmail.settings.basic"
)

// Doer sends a request. In a call it is the client providerkit hands over,
// which goes through the gateway; the person's grant is applied there.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Bundle carries the tools' methods. It holds nothing: every call brings
// its own client.
type Bundle struct{}

type callFn func(ctx context.Context, hc Doer, req provider.CallRequest) (provider.CallResult, error)

type service struct {
	slug   string
	scopes []string
	tools  []provider.Tool
	labels []ifc.Label
	call   callFn
}

func services() []*service {
	b := &Bundle{}
	return []*service{
		{slug: "calendar", scopes: []string{scopeCalendar}, tools: calendarTools, call: b.callCalendar},
		{slug: "tasks", scopes: []string{scopeTasks}, tools: tasksTools, call: b.callTasks},
		{slug: "drive", scopes: []string{scopeDriveRO}, tools: driveTools, call: b.callDrive},
		{slug: "gmail", scopes: []string{scopeGmailModify, scopeGmailSettings}, tools: gmailTools, call: b.callGmail, labels: []ifc.Label{"google", "pii"}},
	}
}

// ---- tool helpers --------------------------------------------------------------

func schema(s string) json.RawMessage { return json.RawMessage(s) }

var (
	readOnly    = provider.Annotations{ReadOnly: true, Idempotent: true}
	writes      = provider.Annotations{}
	idempotentW = provider.Annotations{Idempotent: true}
	destructive = provider.Annotations{Destructive: true}
	// sends reaches people outside the account: irreversible, open world.
	sends = provider.Annotations{Destructive: true, OpenWorld: true}
)

var driveTools = []provider.Tool{
	{Name: "search_files", Title: "Search Drive files",
		Description: "Search or list Drive files with a Drive query string (e.g. name contains 'roadmap', mimeType = 'application/pdf', modifiedTime > '2026-01-01'). No query lists every file not in the trash. include_shared_drives also searches shared drives." + pagingNote,
		InputSchema: schema(`{"type":"object","properties":{
			"query":{"type":"string","description":"Drive query syntax. Empty lists every file not in the trash."},
			"order_by":{"type":"string","description":"e.g. modifiedTime desc, name, createdTime."},
			"include_shared_drives":{"type":"boolean","default":false},
			"page_size":{"type":"integer","minimum":1,"maximum":1000,"default":100},
			"page_token":{"type":"string"}}}`),
		Annotations: readOnly},
	{Name: "get_file_metadata", Title: "Get file metadata",
		InputSchema: schema(`{"type":"object","properties":{"file_id":{"type":"string"}},"required":["file_id"]}`),
		Annotations: readOnly},
}

func (b *Bundle) callDrive(ctx context.Context, hc Doer, req provider.CallRequest) (provider.CallResult, error) {
	switch req.Tool {
	case "search_files":
		var a struct {
			Query        string `json:"query"`
			OrderBy      string `json:"order_by"`
			SharedDrives bool   `json:"include_shared_drives"`
			PageSize     int    `json:"page_size"`
			pageArgs
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.Query == "" {
			a.Query = "trashed = false"
		}
		// nextPageToken has to be in the field mask, or Drive never sends it
		// and the first page looks like the last.
		q := query{"q": a.Query, "fields": "nextPageToken,files(id,name,mimeType,modifiedTime,owners(emailAddress),webViewLink)"}
		if a.OrderBy != "" {
			q["orderBy"] = a.OrderBy
		}
		if a.SharedDrives {
			q["supportsAllDrives"], q["includeItemsFromAllDrives"], q["corpora"] = "true", "true", "allDrives"
		}
		return b.get(ctx, hc, apiBase+"/drive/v3/files?"+paged(q, "pageSize", a.PageSize, 1000, a.PageToken).encode(), "files")
	case "get_file_metadata":
		var a struct {
			FileID string `json:"file_id"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		return b.get(ctx, hc, apiBase+"/drive/v3/files/"+pathEsc(a.FileID)+"?fields=*", "")
	}
	return provider.CallResult{}, fmt.Errorf("google/drive: unknown tool %q", req.Tool)
}

// ---- plumbing ------------------------------------------------------------------

func args(req provider.CallRequest, into any) error {
	if len(req.Arguments) == 0 {
		return nil
	}
	if err := json.Unmarshal(req.Arguments, into); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// errResult is a tool error: the call ran, the answer is "no, because".
func errResult(err error) provider.CallResult {
	return provider.CallResult{IsError: true, Content: []provider.Content{{Type: "text", Text: err.Error()}}}
}

func errorf(format string, a ...any) provider.CallResult { return errResult(fmt.Errorf(format, a...)) }

// apiError is a non-2xx answer from Google, or from the gateway refusing
// the request, carried as a value so a multi-step tool can turn it into a
// tool error at the right moment.
type apiError struct {
	Status int
	Body   []byte
}

func (e *apiError) Error() string {
	return fmt.Sprintf("google api %d: %s", e.Status, truncate(e.Body, 512))
}

// object is a JSON object the tools build and read.
type object = map[string]any

// call performs one request and returns the JSON answer. A non-2xx answer
// is an *apiError; anything else is a transport error. There is no
// Authorization header: the gateway puts the person's grant on it.
func (b *Bundle) call(ctx context.Context, hc Doer, method, u string, body any) (json.RawMessage, error) {
	if hc == nil {
		return nil, errors.New("google: no HTTP client for this call")
	}
	var rdr io.Reader
	if body != nil {
		enc, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("google: encode: %w", err)
		}
		rdr = bytesReader(enc)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("google: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google: %s %s: %w", method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("google: read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &apiError{Status: resp.StatusCode, Body: raw}
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return raw, nil
}

// callObj is call with the answer decoded.
func (b *Bundle) callObj(ctx context.Context, hc Doer, method, u string, body any) (object, error) {
	raw, err := b.call(ctx, hc, method, u, body)
	if err != nil {
		return nil, err
	}
	var out object
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("google: decode: %w", err)
	}
	return out, nil
}

// result turns an answer, or an error, into the tool result: an API refusal
// and a validation failure are tool errors (isError) the agent can act on;
// only a transport failure is a Go error.
func result(raw json.RawMessage, err error) (provider.CallResult, error) {
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			return errResult(ae), nil
		}
		return provider.CallResult{}, err
	}
	return provider.CallResult{StructuredContent: raw, Content: []provider.Content{{Type: "text", Text: string(truncate(raw, 16<<10))}}}, nil
}

// objResult is result for an object the tool built.
func objResult(o object) provider.CallResult {
	raw, _ := json.Marshal(o)
	res, _ := result(raw, nil)
	return res
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append(append([]byte{}, b[:n]...), []byte("…")...)
}
