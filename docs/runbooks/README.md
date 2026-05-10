# Per-connector runbooks

Operator playbooks for the 12 production connectors that ship with
the Phase 7 catalog. Each runbook follows the same shape so an
on-call engineer can navigate to the relevant section without
re-reading the prose:

1. **Auth model** — credential shape, scopes, where the token comes
   from upstream, and how the connector talks to the API
   (`internal/connector/<name>/<name>.go`).
2. **Credential rotation** — the sequence of admin-portal +
   upstream actions to swap a credential without dropping in-flight
   ingestion.
3. **Quota / rate-limit incidents** — the headers to watch, the
   typical 429 / 503 surface, and the platform-side backoff knob
   (`internal/admin/ratelimit.go` Redis token bucket).
4. **Outage detection & recovery** — what to look for in
   `internal/admin/health.go` (last-success, lag, error counts) and
   how to drain → reconnect cleanly.
5. **Common error codes** — the upstream HTTP / RPC codes the
   connector forwards as `connector.Err*` plus their remediation.

| Connector | Runbook | Auth | Webhook | Delta |
|-----------|---------|------|---------|-------|
| Google Drive (`google_drive`) | [`googledrive.md`](googledrive.md) | OAuth 2 access token | poll | `changes.list` |
| Slack (`slack`) | [`slack.md`](slack.md) | Bot OAuth token | Events API | `oldest`/`latest` |
| SharePoint Online (`sharepoint`) | [`sharepoint.md`](sharepoint.md) | Azure AD OAuth (Graph) | poll | Graph delta token |
| OneDrive (`onedrive`) | [`onedrive.md`](onedrive.md) | Azure AD OAuth (Graph) | poll | Graph delta token |
| Dropbox (`dropbox`) | [`dropbox.md`](dropbox.md) | OAuth 2 access token | poll | `list_folder/continue` |
| Box (`box`) | [`box.md`](box.md) | OAuth 2 access token | poll | `events` stream |
| Notion (`notion`) | [`notion.md`](notion.md) | Internal integration token | poll | `last_edited_time` |
| Confluence Cloud (`confluence`) | [`confluence.md`](confluence.md) | Email + API token (Basic) | poll | CQL `lastModified` |
| Jira Cloud (`jira`) | [`jira.md`](jira.md) | Email + API token (Basic) | Jira webhooks | JQL `updated` |
| GitHub (`github`) | [`github.md`](github.md) | Personal access token | GitHub webhooks | `since` filter |
| GitLab (`gitlab`) | [`gitlab.md`](gitlab.md) | Personal access token | GitLab webhooks | `updated_after` |
| Microsoft Teams (`teams`) | [`teams.md`](teams.md) | Azure AD OAuth (Graph) | Graph change notifications | `messages/delta` |

## Cross-cutting notes

- The platform-side **AES-GCM credential envelope** lives in
  [`internal/credential/`](../../internal/credential/). Every
  credential file the runbooks below mention is encrypted at rest;
  the admin-portal `PATCH /v1/admin/sources/:id/credentials`
  re-encrypts the payload before persisting it.
- The **per-source rate limiter**
  ([`internal/admin/ratelimit.go`](../../internal/admin/ratelimit.go))
  stops the connector from melting an upstream tenant during a
  retry storm. When you see an upstream 429 burst, the platform-side
  Redis token bucket usually clamps the next call before it leaves
  the box — no operator action needed unless the bucket is sized
  too aggressively.
- The **forget-on-removal worker**
  ([`internal/admin/forget_worker.go`](../../internal/admin/forget_worker.go))
  guarantees that disconnecting a source drops the derived chunks /
  embeddings / graph nodes / memory entries in Qdrant + Postgres +
  FalkorDB + Tantivy + Redis with a fenced lease so a re-add cannot
  race with deletion.
- Source health (last-success timestamp, lag, error counters) is
  surfaced in the admin portal via
  [`internal/admin/health.go`](../../internal/admin/health.go) —
  every runbook below points back to this dashboard before paging.
- Connector lifecycle audit actions
  (`source.connected`, `source.sync_started`, `chunk.indexed`,
  `source.purged`) are emitted via
  [`internal/audit/model.go`](../../internal/audit/model.go) so
  every action below leaves an audit trail.
