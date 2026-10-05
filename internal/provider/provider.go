// Package provider is the shape the tools were written against inside the
// gateway, kept so they moved here unchanged. main.go turns it into
// providerkit's.
package provider

import (
	"encoding/json"

	"github.com/mcplocker/provider-google/internal/ifc"
	"github.com/mcplocker/server/pkg/providerkit"
)

// Tool is one tool's description; the service's call function runs it.
type Tool struct {
	Name         string
	Title        string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  Annotations
	// Labels are information-flow labels. The gateway takes them from the
	// manifest; they stay here as the record of what a tool returns.
	Labels []ifc.Label
}

// Annotations are the MCP tool hints.
type Annotations = providerkit.Annotations

// Content is one MCP content block.
type Content = providerkit.Content

// CallRequest is one call.
type CallRequest struct {
	Tool      string
	Arguments json.RawMessage
}

// CallResult is what a call returns.
type CallResult struct {
	Content           []Content
	StructuredContent json.RawMessage
	IsError           bool
}
