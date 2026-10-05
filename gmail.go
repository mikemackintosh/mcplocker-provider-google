package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mcplocker/provider-google/internal/ifc"
	"github.com/mcplocker/provider-google/internal/provider"
)

const gmBase = gmailBase + "/gmail/v1/users/me"

// What the Gmail API leaves to the caller, done here:
//
//   - A filter cannot be edited. update_filter creates the replacement
//     first, then deletes the old one, and returns the new id.
//   - A filter applies to mail that arrives after it exists. Nothing in
//     the API applies it backwards; apply_to_existing here does, by
//     searching with the same criteria and batch-modifying the labels.
//   - Filter actions name label *ids*; people know label names. The tools
//     take names (or ids), resolve them, and create missing user labels.
//     Archive, mark read, star, important, spam and trash are label
//     operations too (INBOX, UNREAD, STARRED, IMPORTANT, SPAM, TRASH).
//   - Forwarding needs a forwarding address the account has verified;
//     the API refuses others with an opaque error, so it is checked first.
//   - A reply threads only if In-Reply-To and References carry the
//     original's Message-ID and threadId is set; Gmail does not do that
//     from a subject. Reply-all is computed here and never includes you.
//   - Sending is separate from drafting, so an approval policy can guard
//     send_draft while drafting stays free.

var pii = []ifc.Label{"google", "pii"}

var gmailTools = []provider.Tool{
	{Name: "search_messages", Title: "Search Gmail",
		Description: "Search messages with Gmail query syntax (from:, subject:, newer_than:, has:attachment, label:, is:unread). Returns ids; get_message for the content. result_size_estimate is Gmail's rough guess, not a count: for an exact number of messages or unread in a label, use get_label." + pagingNote,
		InputSchema: schema(`{"type":"object","properties":{
			"query":{"type":"string","description":"Gmail search syntax. Empty lists every message."},
			"label_ids":{"type":"array","items":{"type":"string"},"description":"Only messages with all of these label ids (e.g. INBOX, UNREAD)."},
			"include_spam_trash":{"type":"boolean","default":false},
			"max_results":{"type":"integer","minimum":1,"maximum":500,"default":100},
			"page_token":{"type":"string"}}}`),
		Annotations: readOnly, Labels: pii},
	{Name: "get_message", Title: "Get a message",
		InputSchema: schema(`{"type":"object","properties":{
			"id":{"type":"string"},
			"format":{"type":"string","enum":["metadata","full"],"default":"metadata"}},"required":["id"]}`),
		Annotations: readOnly, Labels: pii},
	{Name: "list_labels", Title: "List labels",
		Description: "Every label with id, name and type (system or user). Filters and label changes take names or ids. Gmail returns them all at once; get_label has the message counts.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Annotations: readOnly},
	{Name: "get_label", Title: "Get a label and its counts",
		Description: "One label by name or id, with the exact number of messages and threads in it and how many are unread, as Gmail counts them (e.g. label UNREAD or INBOX for your unread total). Use this rather than paging through search_messages to count.",
		InputSchema: schema(`{"type":"object","properties":{"label":{"type":"string","description":"A label name (Inbox, Receipts/2026) or id (INBOX, UNREAD, Label_12)."}},"required":["label"]}`),
		Annotations: readOnly},
	{Name: "create_label", Title: "Create a label",
		Description: "Create a user label. Nested labels are names with slashes (Projects/Apollo); the parent must exist.",
		InputSchema: schema(`{"type":"object","properties":{
			"name":{"type":"string"},
			"label_list_visibility":{"type":"string","enum":["labelShow","labelShowIfUnread","labelHide"],"default":"labelShow"},
			"message_list_visibility":{"type":"string","enum":["show","hide"],"default":"show"}},"required":["name"]}`),
		Annotations: writes},
	{Name: "modify_message_labels", Title: "Change a message's labels",
		Description: "Add and remove labels on messages by name or id. Archive = remove INBOX; mark read = remove UNREAD; star = add STARRED; trash = add TRASH.",
		InputSchema: schema(`{"type":"object","properties":{
			"message_ids":{"type":"array","items":{"type":"string"},"minItems":1},
			"add_labels":{"type":"array","items":{"type":"string"}},
			"remove_labels":{"type":"array","items":{"type":"string"}}},"required":["message_ids"]}`),
		Annotations: idempotentW, Labels: pii},
	{Name: "list_filters", Title: "List mail filters",
		Description: "Every filter: id, criteria, action — with label ids resolved to names in action.add_label_names / remove_label_names.",
		InputSchema: schema(`{"type":"object","properties":{}}`),
		Annotations: readOnly},
	{Name: "get_filter", Title: "Get a filter",
		InputSchema: schema(`{"type":"object","properties":{"filter_id":{"type":"string"}},"required":["filter_id"]}`),
		Annotations: readOnly},
	{Name: "create_filter", Title: "Create a mail filter",
		Description: "Create a filter from criteria and actions. Labels are names or ids; missing user labels are created (create_missing_labels). " +
			"Shortcuts: archive (skip the inbox), mark_read, star, mark_important, never_spam, trash. forward needs an address the account has already verified for forwarding. " +
			"A filter only affects mail that arrives from now on; apply_to_existing also applies the label changes to matching mail already in the mailbox and reports how many messages. " +
			"Needs at least one criterion and one action.",
		InputSchema: schema(`{"type":"object","properties":{
			"from":{"type":"string"},
			"to":{"type":"string"},
			"subject":{"type":"string"},
			"query":{"type":"string","description":"Gmail search terms the message must match."},
			"negated_query":{"type":"string","description":"Gmail search terms the message must not match."},
			"has_attachment":{"type":"boolean"},
			"exclude_chats":{"type":"boolean"},
			"size_larger_than_bytes":{"type":"integer","minimum":1},
			"size_smaller_than_bytes":{"type":"integer","minimum":1},
			"add_labels":{"type":"array","items":{"type":"string"}},
			"remove_labels":{"type":"array","items":{"type":"string"}},
			"archive":{"type":"boolean"},
			"mark_read":{"type":"boolean"},
			"star":{"type":"boolean"},
			"mark_important":{"type":"boolean"},
			"never_spam":{"type":"boolean"},
			"trash":{"type":"boolean"},
			"forward":{"type":"string","format":"email","description":"A verified forwarding address."},
			"create_missing_labels":{"type":"boolean","default":true},
			"apply_to_existing":{"type":"boolean","default":false}}}`),
		Annotations: writes},
	{Name: "update_filter", Title: "Replace a mail filter",
		Description: "Gmail filters cannot be edited, so this creates a new filter from the old one with your changes applied, then deletes the old one, and returns the new filter (its id changes). Same fields as create_filter; pass only what changes. clear_criteria / clear_actions drop a field by name.",
		InputSchema: schema(`{"type":"object","properties":{
			"filter_id":{"type":"string"},
			"from":{"type":"string"},
			"to":{"type":"string"},
			"subject":{"type":"string"},
			"query":{"type":"string"},
			"negated_query":{"type":"string"},
			"has_attachment":{"type":"boolean"},
			"exclude_chats":{"type":"boolean"},
			"size_larger_than_bytes":{"type":"integer"},
			"size_smaller_than_bytes":{"type":"integer"},
			"add_labels":{"type":"array","items":{"type":"string"},"description":"Replaces the labels the filter adds."},
			"remove_labels":{"type":"array","items":{"type":"string"},"description":"Replaces the labels the filter removes."},
			"archive":{"type":"boolean"},
			"mark_read":{"type":"boolean"},
			"star":{"type":"boolean"},
			"mark_important":{"type":"boolean"},
			"never_spam":{"type":"boolean"},
			"trash":{"type":"boolean"},
			"forward":{"type":"string"},
			"clear_criteria":{"type":"array","items":{"type":"string","enum":["from","to","subject","query","negated_query","has_attachment","exclude_chats","size"]}},
			"clear_actions":{"type":"array","items":{"type":"string","enum":["add_labels","remove_labels","forward"]}},
			"create_missing_labels":{"type":"boolean","default":true},
			"apply_to_existing":{"type":"boolean","default":false}},"required":["filter_id"]}`),
		Annotations: idempotentW},
	{Name: "delete_filter", Title: "Delete a mail filter",
		Description: "Delete a filter. Mail it already labelled stays as it is.",
		InputSchema: schema(`{"type":"object","properties":{"filter_id":{"type":"string"}},"required":["filter_id"]}`),
		Annotations: destructive},
	{Name: "list_drafts", Title: "List drafts",
		Description: "Drafts, optionally matching a Gmail search." + pagingNote,
		InputSchema: schema(`{"type":"object","properties":{
			"query":{"type":"string"},
			"max_results":{"type":"integer","minimum":1,"maximum":500,"default":100},
			"page_token":{"type":"string"}}}`),
		Annotations: readOnly, Labels: pii},
	{Name: "get_draft", Title: "Get a draft",
		InputSchema: schema(`{"type":"object","properties":{
			"draft_id":{"type":"string"},
			"format":{"type":"string","enum":["metadata","full"],"default":"full"}},"required":["draft_id"]}`),
		Annotations: readOnly, Labels: pii},
	{Name: "create_draft", Title: "Draft an email",
		Description: "Write a draft; nothing is sent. Plain text and/or HTML bodies (both = multipart/alternative). " +
			"To reply, pass reply_to_message_id: the draft joins that thread with In-Reply-To/References set, Subject prefixed Re:, and recipients taken from the original (its Reply-To or From; reply_all adds its To and Cc, never you) unless you pass to/cc yourself. " +
			"Returns the draft id, message id and thread id; send_draft sends it.",
		InputSchema: schema(`{"type":"object","properties":{
			"to":{"type":"array","items":{"type":"string"},"description":"Addresses, 'Name <a@b>' allowed."},
			"cc":{"type":"array","items":{"type":"string"}},
			"bcc":{"type":"array","items":{"type":"string"}},
			"subject":{"type":"string"},
			"body_text":{"type":"string"},
			"body_html":{"type":"string"},
			"reply_to_message_id":{"type":"string","description":"Gmail message id to reply to."},
			"reply_all":{"type":"boolean","default":false}}}`),
		Annotations: writes, Labels: pii},
	{Name: "update_draft", Title: "Rewrite a draft",
		Description: "Replace a draft's content; the draft id is kept and it stays in its thread. Fields as create_draft; unspecified recipients and subject are taken from the current draft.",
		InputSchema: schema(`{"type":"object","properties":{
			"draft_id":{"type":"string"},
			"to":{"type":"array","items":{"type":"string"}},
			"cc":{"type":"array","items":{"type":"string"}},
			"bcc":{"type":"array","items":{"type":"string"}},
			"subject":{"type":"string"},
			"body_text":{"type":"string"},
			"body_html":{"type":"string"}},"required":["draft_id"]}`),
		Annotations: idempotentW, Labels: pii},
	{Name: "delete_draft", Title: "Delete a draft",
		InputSchema: schema(`{"type":"object","properties":{"draft_id":{"type":"string"}},"required":["draft_id"]}`),
		Annotations: destructive},
	{Name: "send_draft", Title: "Send a draft",
		Description: "Send an existing draft as it is. Irreversible: the mail leaves the account. Review the draft with get_draft first.",
		InputSchema: schema(`{"type":"object","properties":{"draft_id":{"type":"string"}},"required":["draft_id"]}`),
		Annotations: sends, Labels: pii},
	{Name: "send_message", Title: "Send an email",
		Description: "Compose and send in one step, with the same fields and reply handling as create_draft. Irreversible. Prefer create_draft then send_draft when a person should see it first.",
		InputSchema: schema(`{"type":"object","properties":{
			"to":{"type":"array","items":{"type":"string"}},
			"cc":{"type":"array","items":{"type":"string"}},
			"bcc":{"type":"array","items":{"type":"string"}},
			"subject":{"type":"string"},
			"body_text":{"type":"string"},
			"body_html":{"type":"string"},
			"reply_to_message_id":{"type":"string"},
			"reply_all":{"type":"boolean","default":false}}}`),
		Annotations: sends, Labels: pii},
}

func (b *Bundle) callGmail(ctx context.Context, hc Doer, req provider.CallRequest) (provider.CallResult, error) {
	switch req.Tool {
	case "search_messages":
		var a struct {
			Query       string   `json:"query"`
			LabelIDs    []string `json:"label_ids"`
			IncludeSpam bool     `json:"include_spam_trash"`
			MaxResults  int      `json:"max_results"`
			pageArgs
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		v := url.Values{}
		for k, val := range paged(query{}, "maxResults", a.MaxResults, 500, a.PageToken) {
			v.Set(k, val)
		}
		if a.Query != "" {
			v.Set("q", a.Query)
		}
		if a.IncludeSpam {
			v.Set("includeSpamTrash", "true")
		}
		// labelIds repeats, which query (one value per key) cannot say.
		for _, l := range a.LabelIDs {
			v.Add("labelIds", l)
		}
		return b.get(ctx, hc, gmBase+"/messages?"+v.Encode(), "messages")
	case "get_message":
		var a struct {
			ID     string `json:"id"`
			Format string `json:"format"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.Format == "" {
			a.Format = "metadata"
		}
		return b.get(ctx, hc, gmBase+"/messages/"+pathEsc(a.ID)+"?"+query{"format": a.Format}.encode(), "")
	case "list_labels":
		return b.get(ctx, hc, gmBase+"/labels", "labels")
	case "get_label":
		var a struct {
			Label string `json:"label"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if strings.TrimSpace(a.Label) == "" {
			return errorf("label is required"), nil
		}
		ix, err := b.labelIndex(ctx, hc)
		if err != nil {
			return result(nil, err)
		}
		ids, err := ix.resolve(ctx, b, hc, []string{a.Label}, false)
		if err != nil {
			return errResult(err), nil
		}
		return result(b.call(ctx, hc, http.MethodGet, gmBase+"/labels/"+pathEsc(ids[0]), nil))
	case "create_label":
		var a struct {
			Name string `json:"name"`
			LLV  string `json:"label_list_visibility"`
			MLV  string `json:"message_list_visibility"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if strings.TrimSpace(a.Name) == "" {
			return errorf("name is required"), nil
		}
		return result(b.createLabel(ctx, hc, a.Name, a.LLV, a.MLV))
	case "modify_message_labels":
		var a struct {
			IDs    []string `json:"message_ids"`
			Add    []string `json:"add_labels"`
			Remove []string `json:"remove_labels"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if len(a.IDs) == 0 || (len(a.Add) == 0 && len(a.Remove) == 0) {
			return errorf("message_ids and at least one of add_labels / remove_labels are required"), nil
		}
		labels, err := b.labelIndex(ctx, hc)
		if err != nil {
			return result(nil, err)
		}
		add, err := labels.resolve(ctx, b, hc, a.Add, true)
		if err != nil {
			return errResult(err), nil
		}
		rem, err := labels.resolve(ctx, b, hc, a.Remove, false)
		if err != nil {
			return errResult(err), nil
		}
		if err := b.batchModify(ctx, hc, a.IDs, add, rem); err != nil {
			return result(nil, err)
		}
		return objResult(object{"modified": len(a.IDs), "added_label_ids": add, "removed_label_ids": rem}), nil
	case "list_filters":
		return b.listFilters(ctx, hc)
	case "get_filter":
		var a struct {
			ID string `json:"filter_id"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		return result(b.call(ctx, hc, http.MethodGet, gmBase+"/settings/filters/"+pathEsc(a.ID), nil))
	case "create_filter":
		var a filterArgs
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		return b.createFilter(ctx, hc, a, nil)
	case "update_filter":
		var a filterArgs
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.FilterID == "" {
			return errorf("filter_id is required"), nil
		}
		old, err := b.callObj(ctx, hc, http.MethodGet, gmBase+"/settings/filters/"+pathEsc(a.FilterID), nil)
		if err != nil {
			return result(nil, err)
		}
		return b.createFilter(ctx, hc, a, old)
	case "delete_filter":
		var a struct {
			ID string `json:"filter_id"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if _, err := b.call(ctx, hc, http.MethodDelete, gmBase+"/settings/filters/"+pathEsc(a.ID), nil); err != nil {
			return result(nil, err)
		}
		return objResult(object{"deleted": true, "filter_id": a.ID}), nil
	case "list_drafts":
		var a struct {
			Query      string `json:"query"`
			MaxResults int    `json:"max_results"`
			pageArgs
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		q := query{}
		if a.Query != "" {
			q["q"] = a.Query
		}
		return b.get(ctx, hc, gmBase+"/drafts?"+paged(q, "maxResults", a.MaxResults, 500, a.PageToken).encode(), "drafts")
	case "get_draft":
		var a struct {
			ID     string `json:"draft_id"`
			Format string `json:"format"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.Format == "" {
			a.Format = "full"
		}
		return b.get(ctx, hc, gmBase+"/drafts/"+pathEsc(a.ID)+"?"+query{"format": a.Format}.encode(), "")
	case "create_draft", "send_message":
		var a composeArgs
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		msg, threadID, err := b.compose(ctx, hc, a, nil)
		if err != nil {
			return errResult(err), nil
		}
		body := object{"raw": msg.raw()}
		if threadID != "" {
			body["threadId"] = threadID
		}
		if req.Tool == "send_message" {
			return result(b.call(ctx, hc, http.MethodPost, gmBase+"/messages/send", body))
		}
		return result(b.call(ctx, hc, http.MethodPost, gmBase+"/drafts", object{"message": body}))
	case "update_draft":
		var a composeArgs
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.DraftID == "" {
			return errorf("draft_id is required"), nil
		}
		cur, err := b.callObj(ctx, hc, http.MethodGet, gmBase+"/drafts/"+pathEsc(a.DraftID)+"?format=metadata", nil)
		if err != nil {
			return result(nil, err)
		}
		msg, threadID, err := b.compose(ctx, hc, a, cur)
		if err != nil {
			return errResult(err), nil
		}
		body := object{"raw": msg.raw()}
		if threadID != "" {
			body["threadId"] = threadID
		}
		return result(b.call(ctx, hc, http.MethodPut, gmBase+"/drafts/"+pathEsc(a.DraftID), object{"id": a.DraftID, "message": body}))
	case "delete_draft":
		var a struct {
			ID string `json:"draft_id"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if _, err := b.call(ctx, hc, http.MethodDelete, gmBase+"/drafts/"+pathEsc(a.ID), nil); err != nil {
			return result(nil, err)
		}
		return objResult(object{"deleted": true, "draft_id": a.ID}), nil
	case "send_draft":
		var a struct {
			ID string `json:"draft_id"`
		}
		if err := args(req, &a); err != nil {
			return errResult(err), nil
		}
		if a.ID == "" {
			return errorf("draft_id is required"), nil
		}
		return result(b.call(ctx, hc, http.MethodPost, gmBase+"/drafts/send", object{"id": a.ID}))
	}
	return provider.CallResult{}, fmt.Errorf("google/gmail: unknown tool %q", req.Tool)
}

// ---- labels --------------------------------------------------------------------

// systemLabels are the ids Gmail fixes; people say the name.
var systemLabels = map[string]string{
	"INBOX": "INBOX", "SPAM": "SPAM", "TRASH": "TRASH", "UNREAD": "UNREAD", "STARRED": "STARRED", "IMPORTANT": "IMPORTANT",
	"SENT": "SENT", "DRAFT": "DRAFT", "CHAT": "CHAT",
	"CATEGORY_PERSONAL": "CATEGORY_PERSONAL", "CATEGORY_SOCIAL": "CATEGORY_SOCIAL", "CATEGORY_PROMOTIONS": "CATEGORY_PROMOTIONS",
	"CATEGORY_UPDATES": "CATEGORY_UPDATES", "CATEGORY_FORUMS": "CATEGORY_FORUMS",
}

type labelIndex struct {
	byID   map[string]string // id → name
	byName map[string]string // lower(name) → id
}

func (b *Bundle) labelIndex(ctx context.Context, hc Doer) (*labelIndex, error) {
	o, err := b.callObj(ctx, hc, http.MethodGet, gmBase+"/labels", nil)
	if err != nil {
		return nil, err
	}
	ix := &labelIndex{byID: map[string]string{}, byName: map[string]string{}}
	if arr, ok := o["labels"].([]any); ok {
		for _, x := range arr {
			l, _ := x.(object)
			lid, _ := l["id"].(string)
			name, _ := l["name"].(string)
			if lid != "" && name != "" {
				ix.byID[lid] = name
				ix.byName[strings.ToLower(name)] = lid
			}
		}
	}
	return ix, nil
}

// resolve turns names or ids into ids. A system name (inbox, Trash) maps
// to its fixed id; an unknown user label is created when create is set,
// else it is an error naming the label.
func (ix *labelIndex) resolve(ctx context.Context, b *Bundle, hc Doer, in []string, create bool) ([]string, error) {
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if lid, ok := systemLabels[strings.ToUpper(s)]; ok {
			out = append(out, lid)
			continue
		}
		if _, ok := ix.byID[s]; ok {
			out = append(out, s)
			continue
		}
		if lid, ok := ix.byName[strings.ToLower(s)]; ok {
			out = append(out, lid)
			continue
		}
		if !create {
			return nil, fmt.Errorf("no label named %q (list_labels shows what exists)", s)
		}
		raw, err := b.createLabel(ctx, hc, s, "", "")
		if err != nil {
			return nil, fmt.Errorf("create label %q: %w", s, err)
		}
		var l struct {
			ID string `json:"id"`
		}
		_ = unmarshal(raw, &l)
		if l.ID == "" {
			return nil, fmt.Errorf("create label %q: no id returned", s)
		}
		ix.byID[l.ID], ix.byName[strings.ToLower(s)] = s, l.ID
		out = append(out, l.ID)
	}
	return out, nil
}

func (ix *labelIndex) names(ids []any) []string {
	out := []string{}
	for _, x := range ids {
		lid, _ := x.(string)
		if n, ok := ix.byID[lid]; ok {
			out = append(out, n)
		} else {
			out = append(out, lid)
		}
	}
	return out
}

func (b *Bundle) createLabel(ctx context.Context, hc Doer, name, llv, mlv string) ([]byte, error) {
	if llv == "" {
		llv = "labelShow"
	}
	if mlv == "" {
		mlv = "show"
	}
	return b.call(ctx, hc, http.MethodPost, gmBase+"/labels", object{"name": name, "labelListVisibility": llv, "messageListVisibility": mlv})
}

// batchModify applies label changes to messages, 1000 per request as the
// API caps it.
func (b *Bundle) batchModify(ctx context.Context, hc Doer, ids, add, remove []string) error {
	for len(ids) > 0 {
		n := min(len(ids), 1000)
		body := object{"ids": ids[:n]}
		if len(add) > 0 {
			body["addLabelIds"] = add
		}
		if len(remove) > 0 {
			body["removeLabelIds"] = remove
		}
		if _, err := b.call(ctx, hc, http.MethodPost, gmBase+"/messages/batchModify", body); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

// ---- filters -------------------------------------------------------------------

type filterArgs struct {
	FilterID            string   `json:"filter_id"`
	From                *string  `json:"from"`
	To                  *string  `json:"to"`
	Subject             *string  `json:"subject"`
	Query               *string  `json:"query"`
	NegatedQuery        *string  `json:"negated_query"`
	HasAttachment       *bool    `json:"has_attachment"`
	ExcludeChats        *bool    `json:"exclude_chats"`
	SizeLarger          *int     `json:"size_larger_than_bytes"`
	SizeSmaller         *int     `json:"size_smaller_than_bytes"`
	AddLabels           []string `json:"add_labels"`
	RemoveLabels        []string `json:"remove_labels"`
	Archive             *bool    `json:"archive"`
	MarkRead            *bool    `json:"mark_read"`
	Star                *bool    `json:"star"`
	MarkImportant       *bool    `json:"mark_important"`
	NeverSpam           *bool    `json:"never_spam"`
	Trash               *bool    `json:"trash"`
	Forward             *string  `json:"forward"`
	ClearCriteria       []string `json:"clear_criteria"`
	ClearActions        []string `json:"clear_actions"`
	CreateMissingLabels *bool    `json:"create_missing_labels"`
	ApplyToExisting     bool     `json:"apply_to_existing"`
}

// createFilter builds the filter from the args laid over old (nil for a
// new one), resolves labels, checks forwarding, creates it, deletes old,
// and optionally applies it to existing mail.
func (b *Bundle) createFilter(ctx context.Context, hc Doer, a filterArgs, old object) (provider.CallResult, error) {
	criteria, action := object{}, object{}
	if old != nil {
		if c, ok := old["criteria"].(object); ok {
			for k, v := range c {
				criteria[k] = v
			}
		}
		if ac, ok := old["action"].(object); ok {
			for k, v := range ac {
				action[k] = v
			}
		}
	}
	for _, c := range a.ClearCriteria {
		switch c {
		case "size":
			delete(criteria, "size")
			delete(criteria, "sizeComparison")
		case "has_attachment":
			delete(criteria, "hasAttachment")
		case "exclude_chats":
			delete(criteria, "excludeChats")
		case "negated_query":
			delete(criteria, "negatedQuery")
		default:
			delete(criteria, c)
		}
	}
	setStr := func(k string, v *string) {
		if v != nil {
			if *v == "" {
				delete(criteria, k)
			} else {
				criteria[k] = *v
			}
		}
	}
	setStr("from", a.From)
	setStr("to", a.To)
	setStr("subject", a.Subject)
	setStr("query", a.Query)
	setStr("negatedQuery", a.NegatedQuery)
	if a.HasAttachment != nil {
		criteria["hasAttachment"] = *a.HasAttachment
	}
	if a.ExcludeChats != nil {
		criteria["excludeChats"] = *a.ExcludeChats
	}
	if a.SizeLarger != nil && a.SizeSmaller != nil {
		return errorf("size_larger_than_bytes and size_smaller_than_bytes are exclusive"), nil
	}
	if a.SizeLarger != nil {
		criteria["size"], criteria["sizeComparison"] = *a.SizeLarger, "larger"
	}
	if a.SizeSmaller != nil {
		criteria["size"], criteria["sizeComparison"] = *a.SizeSmaller, "smaller"
	}
	if len(criteria) == 0 {
		return errorf("a filter needs at least one criterion (from, to, subject, query, negated_query, has_attachment, size)"), nil
	}

	labels, err := b.labelIndex(ctx, hc)
	if err != nil {
		return result(nil, err)
	}
	create := a.CreateMissingLabels == nil || *a.CreateMissingLabels
	// The label sets: start from the old filter's, replace when given,
	// then apply the shortcuts on top.
	addSet, remSet := toStringSet(action["addLabelIds"]), toStringSet(action["removeLabelIds"])
	for _, c := range a.ClearActions {
		switch c {
		case "add_labels":
			addSet = map[string]bool{}
		case "remove_labels":
			remSet = map[string]bool{}
		case "forward":
			delete(action, "forward")
		}
	}
	if a.AddLabels != nil {
		ids, err := labels.resolve(ctx, b, hc, a.AddLabels, create)
		if err != nil {
			return errResult(err), nil
		}
		addSet = toSet(ids)
	}
	if a.RemoveLabels != nil {
		ids, err := labels.resolve(ctx, b, hc, a.RemoveLabels, create)
		if err != nil {
			return errResult(err), nil
		}
		remSet = toSet(ids)
	}
	shortcut := func(flag *bool, add bool, lid string) {
		if flag == nil {
			return
		}
		target, other := remSet, addSet
		if add {
			target, other = addSet, remSet
		}
		if *flag {
			target[lid] = true
			delete(other, lid)
		} else {
			delete(target, lid)
		}
	}
	shortcut(a.Archive, false, "INBOX")
	shortcut(a.MarkRead, false, "UNREAD")
	shortcut(a.Star, true, "STARRED")
	shortcut(a.MarkImportant, true, "IMPORTANT")
	shortcut(a.NeverSpam, false, "SPAM")
	shortcut(a.Trash, true, "TRASH")
	if a.Forward != nil {
		if *a.Forward == "" {
			delete(action, "forward")
		} else {
			if err := b.checkForwarding(ctx, hc, *a.Forward); err != nil {
				return errResult(err), nil
			}
			action["forward"] = *a.Forward
		}
	}
	addIDs, remIDs := sorted(addSet), sorted(remSet)
	delete(action, "addLabelIds")
	delete(action, "removeLabelIds")
	if len(addIDs) > 0 {
		action["addLabelIds"] = addIDs
	}
	if len(remIDs) > 0 {
		action["removeLabelIds"] = remIDs
	}
	if len(action) == 0 {
		return errorf("a filter needs at least one action (labels, archive, mark_read, star, mark_important, never_spam, trash, forward)"), nil
	}

	created, err := b.callObj(ctx, hc, http.MethodPost, gmBase+"/settings/filters", object{"criteria": criteria, "action": action})
	if err != nil {
		return result(nil, err)
	}
	out := object{"filter": created, "add_label_names": labels.names(anyList(addIDs)), "remove_label_names": labels.names(anyList(remIDs))}
	if old != nil {
		oldID, _ := old["id"].(string)
		if _, err := b.call(ctx, hc, http.MethodDelete, gmBase+"/settings/filters/"+pathEsc(oldID), nil); err != nil {
			out["warning"] = fmt.Sprintf("the replacement %v was created but the old filter %s could not be deleted: %v", created["id"], oldID, err)
		} else {
			out["replaced_filter_id"] = oldID
		}
	}
	if a.ApplyToExisting && (len(addIDs) > 0 || len(remIDs) > 0) {
		n, err := b.applyToExisting(ctx, hc, criteria, addIDs, remIDs)
		out["applied_to_existing_messages"] = n
		if err != nil {
			out["warning"] = fmt.Sprintf("applied to %d existing messages before an error: %v", n, err)
		}
	}
	return objResult(out), nil
}

// checkForwarding refuses an address the account has not verified; the
// API's own refusal says only "invalid".
func (b *Bundle) checkForwarding(ctx context.Context, hc Doer, addr string) error {
	o, err := b.callObj(ctx, hc, http.MethodGet, gmBase+"/settings/forwardingAddresses", nil)
	if err != nil {
		return err
	}
	if arr, ok := o["forwardingAddresses"].([]any); ok {
		for _, x := range arr {
			f, _ := x.(object)
			e, _ := f["forwardingEmail"].(string)
			st, _ := f["verificationStatus"].(string)
			if strings.EqualFold(e, addr) {
				if st == "accepted" {
					return nil
				}
				return fmt.Errorf("forwarding to %s is set up but not verified (%s); confirm it from Gmail settings first", addr, st)
			}
		}
	}
	return fmt.Errorf("%s is not a verified forwarding address for this account; add and confirm it under Gmail settings → Forwarding first", addr)
}

// filterQuery is the search that matches what a filter's criteria match,
// so the same change can be applied to mail already here.
func filterQuery(c object) string {
	var parts []string
	if v, _ := c["from"].(string); v != "" {
		parts = append(parts, "from:("+v+")")
	}
	if v, _ := c["to"].(string); v != "" {
		parts = append(parts, "to:("+v+")")
	}
	if v, _ := c["subject"].(string); v != "" {
		parts = append(parts, "subject:("+v+")")
	}
	if v, _ := c["query"].(string); v != "" {
		parts = append(parts, "("+v+")")
	}
	if v, _ := c["negatedQuery"].(string); v != "" {
		parts = append(parts, "-("+v+")")
	}
	if v, _ := c["hasAttachment"].(bool); v {
		parts = append(parts, "has:attachment")
	}
	if size, ok := c["size"].(float64); ok && size > 0 {
		if cmp, _ := c["sizeComparison"].(string); cmp == "smaller" {
			parts = append(parts, fmt.Sprintf("smaller:%d", int(size)))
		} else {
			parts = append(parts, fmt.Sprintf("larger:%d", int(size)))
		}
	} else if size, ok := c["size"].(int); ok && size > 0 {
		if cmp, _ := c["sizeComparison"].(string); cmp == "smaller" {
			parts = append(parts, fmt.Sprintf("smaller:%d", size))
		} else {
			parts = append(parts, fmt.Sprintf("larger:%d", size))
		}
	}
	return strings.Join(parts, " ")
}

// applyToExisting labels matching mail already in the mailbox: pages of
// ids from search, batches of 1000 to batchModify, up to a ceiling so one
// call cannot run for an hour on a huge mailbox.
func (b *Bundle) applyToExisting(ctx context.Context, hc Doer, criteria object, add, remove []string) (int, error) {
	const ceiling = 5000
	q := filterQuery(criteria)
	if q == "" {
		return 0, nil
	}
	total, page := 0, ""
	for total < ceiling {
		qs := query{"q": q, "maxResults": "500"}
		if page != "" {
			qs["pageToken"] = page
		}
		o, err := b.callObj(ctx, hc, http.MethodGet, gmBase+"/messages?"+qs.encode(), nil)
		if err != nil {
			return total, err
		}
		ids := []string{}
		if arr, ok := o["messages"].([]any); ok {
			for _, x := range arr {
				m, _ := x.(object)
				if mid, _ := m["id"].(string); mid != "" {
					ids = append(ids, mid)
				}
			}
		}
		if len(ids) == 0 {
			break
		}
		if err := b.batchModify(ctx, hc, ids, add, remove); err != nil {
			return total, err
		}
		total += len(ids)
		page, _ = o["nextPageToken"].(string)
		if page == "" {
			break
		}
	}
	return total, nil
}

func (b *Bundle) listFilters(ctx context.Context, hc Doer) (provider.CallResult, error) {
	o, err := b.callObj(ctx, hc, http.MethodGet, gmBase+"/settings/filters", nil)
	if err != nil {
		return result(nil, err)
	}
	labels, err := b.labelIndex(ctx, hc)
	if err != nil {
		return result(nil, err)
	}
	filters := []object{}
	if arr, ok := o["filter"].([]any); ok {
		for _, x := range arr {
			f, _ := x.(object)
			if ac, ok := f["action"].(object); ok {
				if ids, ok := ac["addLabelIds"].([]any); ok {
					ac["add_label_names"] = labels.names(ids)
				}
				if ids, ok := ac["removeLabelIds"].([]any); ok {
					ac["remove_label_names"] = labels.names(ids)
				}
			}
			filters = append(filters, f)
		}
	}
	return objResult(object{"filters": filters}), nil
}

// ---- drafts --------------------------------------------------------------------

type composeArgs struct {
	DraftID   string   `json:"draft_id"`
	To        []string `json:"to"`
	Cc        []string `json:"cc"`
	Bcc       []string `json:"bcc"`
	Subject   *string  `json:"subject"`
	BodyText  string   `json:"body_text"`
	BodyHTML  string   `json:"body_html"`
	ReplyToID string   `json:"reply_to_message_id"`
	ReplyAll  bool     `json:"reply_all"`
}

// compose builds the message. For a reply it reads the original's headers
// and threads properly; for a draft update it keeps what the draft had
// where the caller passed nothing.
func (b *Bundle) compose(ctx context.Context, hc Doer, a composeArgs, cur object) (message, string, error) {
	var m message
	threadID := ""
	me, err := b.profileEmail(ctx, hc)
	if err != nil {
		return m, "", err
	}
	m.From = me
	if a.To != nil {
		if m.To, err = addresses(a.To); err != nil {
			return m, "", err
		}
	}
	if a.Cc != nil {
		if m.Cc, err = addresses(a.Cc); err != nil {
			return m, "", err
		}
	}
	if a.Bcc != nil {
		if m.Bcc, err = addresses(a.Bcc); err != nil {
			return m, "", err
		}
	}
	if a.Subject != nil {
		m.Subject = *a.Subject
	}
	m.Text, m.HTML = a.BodyText, a.BodyHTML

	if cur != nil {
		// Rewriting a draft: keep its thread, and its headers where the
		// caller passed nothing.
		if msg, ok := cur["message"].(object); ok {
			threadID, _ = msg["threadId"].(string)
			hdr := headersOf(msg)
			if a.To == nil {
				m.To = splitAddresses(hdr["to"])
			}
			if a.Cc == nil {
				m.Cc = splitAddresses(hdr["cc"])
			}
			if a.Bcc == nil {
				m.Bcc = splitAddresses(hdr["bcc"])
			}
			if a.Subject == nil {
				m.Subject = hdr["subject"]
			}
			m.InReplyTo = hdr["in-reply-to"]
			if r := hdr["references"]; r != "" {
				m.References = strings.Fields(r)
			}
		}
	}
	if a.ReplyToID != "" {
		orig, err := b.callObj(ctx, hc, http.MethodGet, gmBase+"/messages/"+pathEsc(a.ReplyToID)+"?"+query{"format": "metadata", "metadataHeaders": "Message-ID,References,Subject,From,To,Cc,Reply-To"}.encode(), nil)
		if err != nil {
			return m, "", err
		}
		threadID, _ = orig["threadId"].(string)
		hdr := headersOf(orig)
		if mid := hdr["message-id"]; mid != "" {
			m.InReplyTo = mid
			m.References = append(strings.Fields(hdr["references"]), mid)
		}
		if a.Subject == nil {
			subj := hdr["subject"]
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(subj)), "re:") {
				subj = "Re: " + subj
			}
			m.Subject = subj
		}
		if a.To == nil {
			replyTo := hdr["reply-to"]
			if replyTo == "" {
				replyTo = hdr["from"]
			}
			m.To = without(splitAddresses(replyTo), me)
			if a.ReplyAll {
				m.To = append(m.To, without(splitAddresses(hdr["to"]), append([]string{me}, m.To...)...)...)
				if a.Cc == nil {
					m.Cc = without(splitAddresses(hdr["cc"]), append([]string{me}, m.To...)...)
				}
			}
			if len(m.To) == 0 {
				// Replying to your own message: it goes back to its recipients.
				m.To = without(splitAddresses(hdr["to"]), me)
			}
		}
	}
	if len(m.To)+len(m.Cc)+len(m.Bcc) == 0 {
		return m, "", fmt.Errorf("no recipients: pass to, or reply_to_message_id")
	}
	if strings.TrimSpace(m.Subject) == "" && a.ReplyToID == "" && cur == nil {
		return m, "", fmt.Errorf("subject is required")
	}
	if m.Text == "" && m.HTML == "" {
		return m, "", fmt.Errorf("body_text or body_html is required")
	}
	return m, threadID, nil
}

// headersOf reads a message resource's payload headers, keys lower-cased.
func headersOf(msg object) map[string]string {
	out := map[string]string{}
	payload, _ := msg["payload"].(object)
	if arr, ok := payload["headers"].([]any); ok {
		for _, x := range arr {
			h, _ := x.(object)
			n, _ := h["name"].(string)
			v, _ := h["value"].(string)
			if n != "" {
				out[strings.ToLower(n)] = v
			}
		}
	}
	return out
}

func (b *Bundle) profileEmail(ctx context.Context, hc Doer) (string, error) {
	o, err := b.callObj(ctx, hc, http.MethodGet, gmBase+"/profile", nil)
	if err != nil {
		return "", err
	}
	e, _ := o["emailAddress"].(string)
	if e == "" {
		return "", fmt.Errorf("google/gmail: could not read your address from the profile")
	}
	return e, nil
}
