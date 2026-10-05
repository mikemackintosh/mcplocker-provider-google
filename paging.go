package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mcplocker/provider-google/internal/provider"
)

// Every list tool pages the same way, because every Google list API does:
// ask for a page, get back a token for the next one, pass it back with the
// same arguments. The tools keep Google's page sizes up to Google's own
// maximum, and hand the token back as next_page_token — absent on the last
// page — so an agent can enumerate a calendar, a Drive or a mailbox to the
// end instead of stopping at whatever fitted on the first page.
const pagingNote = " Results come a page at a time: when the answer has next_page_token, call again with the same arguments and page_token set to it, until it is absent."

// pageArgs is the part of a list tool's arguments that pages.
type pageArgs struct {
	PageToken string `json:"page_token"`
}

// paged sets the page size (clamped to Google's maximum for this API) and
// the page token on a query.
func paged(q query, sizeKey string, size, max int, token string) query {
	if size > max {
		size = max
	}
	if size > 0 {
		q[sizeKey] = fmt.Sprint(size)
	}
	if token != "" {
		q["pageToken"] = token
	}
	return q
}

// get returns a collection under its key ({"items": […]}) so structuredContent
// is an object, as the spec requires, and a shape report.Measure counts —
// with the next page's token and Gmail's size estimate carried over, and an
// empty collection rather than a missing key when there is nothing (Gmail
// leaves "messages" out entirely on no matches).
func (b *Bundle) get(ctx context.Context, hc Doer, u, collectionKey string) (provider.CallResult, error) {
	raw, err := b.call(ctx, hc, http.MethodGet, u, nil)
	if err != nil {
		return result(nil, err)
	}
	if collectionKey == "" {
		return result(raw, nil)
	}
	return result(pageOf(raw, collectionKey), nil)
}

// pageOf reshapes one page of a Google list answer.
func pageOf(raw json.RawMessage, collectionKey string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	out := map[string]json.RawMessage{collectionKey: json.RawMessage("[]")}
	if col, ok := obj[collectionKey]; ok && string(col) != "null" {
		out[collectionKey] = col
	}
	if next, ok := obj["nextPageToken"]; ok {
		var s string
		if json.Unmarshal(next, &s) == nil && s != "" {
			out["next_page_token"] = next
		}
	}
	if est, ok := obj["resultSizeEstimate"]; ok {
		out["result_size_estimate"] = est
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}
