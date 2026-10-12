# provider-google

mcplocker's Google Workspace provider: Calendar, Tasks, Drive (read-only) and Gmail,
through each person's own Google grant. Each service is its own MCP endpoint and asks
only for its own scopes.

It is a **server package**. The gateway runs this image once per person and hands each
tool call a capability that lasts as long as the call. The image sends Google requests
through the gateway, and the gateway applies the grant. The image never holds a Google
token, and the gateway refuses any host the manifest's credential does not name.

The tools carry what the raw APIs leave to the caller:
- a created meeting lists its creator as an attendee;
- invitations go out only when a tool says so;
- a Meet link is requested properly;
- a Gmail filter is replaced, not edited;
- a reply threads with the original's headers.

## Layout

| File | What |
|---|---|
| `mcplocker-provider.yaml` | The manifest: the credential, four services and their scopes, tool tags, setup steps |
| `calendar.go`, `tasks.go`, `gmail.go`, `google.go` | The tools, moved from the gateway's native provider unchanged but for the HTTP client |
| `internal/provider`, `internal/ifc`, `internal/id` | The few gateway shapes those files were written against |
| `main.go` | Serves them with [providerkit](../../mcplocker/pkg/providerkit) |

## Work on it

```sh
make test     # vet and unit tests, including that the manifest tags every tool and asks for every scope
make lint     # the gateway's own manifest check
make smoke    # build the image and check every service answers as the gateway expects
```

These expect a checkout of the gateway at `../../mcplocker` (override with `GATEWAY=`).

## Release

Push a tag `vX.Y.Z` matching `version:` in the manifest. The image goes to
`ghcr.io/mikemackintosh/mcplocker-provider-google-server`, and the signed package to
`ghcr.io/mikemackintosh/mcplocker-provider-google`. A gateway installs it only after an
admin trusts `mikemackintosh` as a publisher (Admin → Providers → Install → Publishers):
the signature check names the trusted GitHub owners, and only `mcplocker` is trusted by default.

## Not yet

- **The Workspace domain hint** (`hd`), which the native provider sent per instance. A
  manifest's OAuth parameters cannot yet come from an organization's setting.
- **Never released.** The workflows have not run.
