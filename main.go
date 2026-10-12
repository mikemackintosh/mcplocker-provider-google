// Command provider-google is mcplocker's Google Workspace provider:
// Calendar, Tasks, Drive (read-only) and Gmail, through each person's own
// Google grant. It runs as a server package in the gateway's hosted runner
// and never holds the grant: every request goes through the gateway, which
// applies it.
package main

import (
	"context"
	"log"

	"github.com/mcplocker/server/pkg/providerkit"
	"github.com/mikemackintosh/mcplocker-provider-google/internal/provider"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	kit := providerkit.New("google", version)
	for _, s := range services() {
		kit.Service(s.slug, s.kitTools()...)
	}
	log.Fatal(kit.ListenAndServe())
}

// kitTools serves a service's tools through providerkit: each call runs the
// service's call function with the call's own client.
func (s *service) kitTools() []providerkit.Tool {
	out := make([]providerkit.Tool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, providerkit.Tool{
			Name: t.Name, Title: t.Title, Description: t.Description,
			InputSchema: t.InputSchema, OutputSchema: t.OutputSchema, Annotations: t.Annotations,
			Handler: func(ctx context.Context, c *providerkit.Call) (providerkit.Result, error) {
				res, err := s.call(ctx, c.HTTP, provider.CallRequest{Tool: c.Tool, Arguments: c.Arguments})
				if err != nil {
					return providerkit.Result{}, err
				}
				r := providerkit.Result{Content: res.Content, IsError: res.IsError}
				if len(res.StructuredContent) > 0 {
					r.StructuredContent = res.StructuredContent
				}
				return r, nil
			},
		})
	}
	return out
}
