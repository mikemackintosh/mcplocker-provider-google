package main

import (
	"net/url"
	"sort"
)

// query is a small ordered url.Values: one value per key, encoded sorted so
// tests can compare URLs.
type query map[string]string

func (q query) encode() string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	v := url.Values{}
	for _, k := range keys {
		v.Set(k, q[k])
	}
	return v.Encode()
}

func pathEsc(s string) string { return url.PathEscape(s) }
