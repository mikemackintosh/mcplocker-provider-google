// Package id makes the unique values Google asks for: a Meet request id, a
// MIME boundary, a Message-ID.
package id

import (
	"crypto/rand"
	"fmt"
)

// ID is 16 random bytes, written as a UUID.
type ID [16]byte

// New returns a random version 4 UUID. Nothing here is stored or sorted,
// so the gateway's time-ordered v7 buys nothing.
func New() ID {
	var b ID
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return b
}

// String formats it as a UUID.
func (i ID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", i[0:4], i[4:6], i[6:8], i[8:10], i[10:16])
}
