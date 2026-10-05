package main

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/mcplocker/provider-google/internal/id"
)

// A message the drafting tools build. Gmail takes the whole thing as an
// RFC 2822 document, base64url-encoded, in the "raw" field; it fills in
// From (the account's own address) but nothing else, so threading and
// encoding are ours to get right.
type message struct {
	From       string
	To         []string
	Cc         []string
	Bcc        []string
	Subject    string
	Text       string
	HTML       string
	InReplyTo  string   // original Message-ID
	References []string // original References + its Message-ID
}

// rfc2822 renders the message. Every body is UTF-8 in base64 so nothing
// depends on the transfer being 8-bit clean; the subject and display
// names are RFC 2047-encoded when they need it.
func (m message) rfc2822() string {
	var b strings.Builder
	h := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	h("From", m.From)
	h("To", strings.Join(m.To, ", "))
	h("Cc", strings.Join(m.Cc, ", "))
	h("Bcc", strings.Join(m.Bcc, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", "<"+id.New().String()+"@mcplocker>")
	h("In-Reply-To", m.InReplyTo)
	h("References", strings.Join(m.References, " "))
	h("MIME-Version", "1.0")
	part := func(ctype, body string) {
		fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n", ctype, wrap76(base64.StdEncoding.EncodeToString([]byte(body))))
	}
	switch {
	case m.HTML != "" && m.Text != "":
		boundary := "=_mcplocker_" + id.New().String()
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		part("text/plain", m.Text)
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		part("text/html", m.HTML)
		fmt.Fprintf(&b, "--%s--\r\n", boundary)
	case m.HTML != "":
		part("text/html", m.HTML)
	default:
		part("text/plain", m.Text)
	}
	return b.String()
}

// raw is what the API's "raw" field takes.
func (m message) raw() string { return base64.URLEncoding.EncodeToString([]byte(m.rfc2822())) }

func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteString("\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}

// addresses parses a list of "Name <a@b>" or "a@b" strings and returns them
// normalised; an unparsable one is an error the tool reports.
func addresses(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		a, err := mail.ParseAddress(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an email address", s)
		}
		out = append(out, a.String())
	}
	return out, nil
}

// splitAddresses parses a header value holding several addresses.
func splitAddresses(header string) []string {
	list, err := mail.ParseAddressList(header)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.String())
	}
	return out
}

func sameAddress(a, b string) bool {
	pa, errA := mail.ParseAddress(a)
	pb, errB := mail.ParseAddress(b)
	if errA != nil || errB != nil {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	return strings.EqualFold(pa.Address, pb.Address)
}

// without drops every address in list that is one of excluded.
func without(list []string, excluded ...string) []string {
	out := list[:0:0]
	for _, a := range list {
		skip := false
		for _, e := range excluded {
			if sameAddress(a, e) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, a)
		}
	}
	return out
}
