# jmap-paperless

Uploads attachments from emails in one JMAP label (e.g. Fastmail `paperless`) to
[paperless-ngx](https://docs.paperless-ngx.com), then removes the label. Runs once and
exits; schedule it with cron, a k8s CronJob, a systemd timer, etc.

Per email:

- Every attachment matching `ALLOWED_TYPES` (by MIME type or file extension) is uploaded.
- All uploads succeed: the `paperless` label is removed. If that would leave the email in
  no mailbox, it is moved to Archive.
- Any upload rejected (4xx), or no allowed attachments: the label is swapped for
  `paperless/failed`. Successful uploads from that email are kept; fix the rest by hand.
- Paperless/JMAP unreachable, 5xx or auth errors: the run aborts with exit code 1 and
  leaves the email labelled for the next run.

## Config

| Env var | Default | |
|---|---|---|
| `JMAP_TOKEN` | required | Fastmail API token with the **Email** scope only |
| `PAPERLESS_URL` | required | e.g. `https://paperless.example.com` |
| `PAPERLESS_TOKEN` | required | API token for the ingest user (see below) |
| `PAPERLESS_TAG_IDS` | | Comma-separated tag IDs to set on every upload |
| `ALLOWED_TYPES` | `application/pdf` | Comma-separated MIME types |
| `LABEL` | `paperless` | Label/mailbox to process |
| `FAILED_LABEL` | `paperless/failed` | Must exist; nested labels use `/` |
| `JMAP_SESSION_URL` | `https://api.fastmail.com/jmap/session` | |

Both labels must exist before the first run.

## Paperless setup

1. Create a tag, e.g. `via-email`, and put its ID in `PAPERLESS_TAG_IDS`.
2. Create a user `mail-ingest` with only the **Document: add** permission, and an API
   token for it.
3. Add a workflow so you own the documents instead of `mail-ingest`:
   - Trigger: **Document Added**, filter **has tags** `via-email`
   - Action: **Assignment**, owner = you
   - Optional second action: **Removal** of the `via-email` tag

## Running

```sh
docker run --rm --env-file jmap-paperless.env ghcr.io/jloh/jmap-paperless:<version>
```

The app does not lock against overlapping runs. Prevent them in the scheduler
(`concurrencyPolicy: Forbid` on a CronJob, or `flock -n` in crontab).

## Development

```sh
mise run test
mise run build
mise run run   # one real run, env loaded from a gitignored .env
goreleaser release --snapshot --clean   # local binaries + images in dist/
```

## Licence

MIT
