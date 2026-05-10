# Privacy strip render contract

## Why

The Phase 4 invariant is that **every retrieval result carries a
structured privacy disclosure**. Operators and end-users must
always see:

* what privacy mode this row was served under,
* where the AI processing happened (device, local API, remote),
* what model tier responded,
* which connector(s) the data came from,
* which policy decision was applied (passthrough / redacted /
  blocked).

The Go-side type lives in `internal/retrieval/privacy_strip.go`
and ships on the wire as `RetrieveHit.privacy_strip`.

## Shape

```json
{
  "mode": "hybrid",
  "processed_where": "device",
  "model_tier": "mid",
  "data_sources": ["google_drive", "slack"],
  "policy_applied": "passthrough"
}
```

| Field | Values | Notes |
|-------|--------|-------|
| `mode` | `no-ai` / `local-only` / `local-api` / `hybrid` / `remote` | matches `policy.PrivacyMode` |
| `processed_where` | `device` / `local-api` / `remote` | where the response was synthesised |
| `model_tier` | `unknown` / `low` / `mid` / `high` | which Bonsai SLM tier served (or "remote" model) |
| `data_sources` | connector ids, sorted alphabetically | tenant-internal connectors only |
| `policy_applied` | `passthrough` / `redacted` / `blocked` | end-state of the privacy filter |

## Render guidance

Keep the strip **always visible** on the result row. Hiding it
behind a tap is acceptable on tight surfaces (mobile list rows)
but the disclosure must remain one tap away.

### Admin portal (kchat-portal, b2c-kchat-portal)

Single-line strip below the title:

```
[mode: hybrid]  [processed_where: device]  [model_tier: mid]  [sources: google_drive, slack]
```

* Render each chip with the colour scheme:
  * green for `passthrough`,
  * amber for `redacted`,
  * red for `blocked`.
* Hover tooltips spell out the meaning of each field.

### Desktop (Skytrack desktop, kchat-desktop)

Two-line strip; the second line carries `policy_applied` so the
state change ("redacted") is unmissable on a wide-format result
card.

```
mode: hybrid   processed_where: device   model_tier: mid
sources: google_drive, slack            policy: passthrough
```

### iOS

Single-row chip group below the result title, max two visible
chips before "+N" overflow. Tapping the chip group expands a
sheet with the full strip.

The chip order MUST match the order in the JSON (mode →
processed_where → model_tier → data_sources → policy_applied).
This means a quick visual scan reads the most-load-bearing field
first.

### Android

Mirror the iOS render. Use `MaterialChipGroup` with
`singleLine=true` and `app:chipScrollX="auto"`. Tapping the chip
group opens a Material `BottomSheet`.

## Internationalisation

The four enum-valued fields (`mode`, `processed_where`,
`model_tier`, `policy_applied`) are NOT translated on the wire;
they ship as their canonical lower-case identifiers. Each client
provides a localised label table keyed by identifier so the
display string matches the user's locale.

`data_sources` ships as connector ids; clients translate ids to
display names using the same translation table the source-management
admin UI uses.

## Accessibility

* The strip's text (or its expanded form on overflow) MUST be
  exposed to the screen reader as a single ordered list, in the
  same order as the JSON.
* Colour MUST NOT be the only signal of the policy state — pair
  colour with an icon (`✓` for passthrough, `~` for redacted,
  `🚫` for blocked).

## Auditing

The strip is the source of truth for the user-facing disclosure;
the audit log carries the full Phase 4 strip on every retrieval
event so a regulator can replay what the user saw.
