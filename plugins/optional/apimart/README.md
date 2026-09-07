# APIMart image task adapter (candidate)

This optional New API v1 plugin converts APIMart submissions, polling responses,
usage facts and image artifacts for `gpt-image-2` and `gpt-image-2-official`.
It is not embedded or automatically enabled, and contains no credentials.

The channel supplies the existing provider key and model mapping. `upstreamModel`
is authoritative for the outgoing model; polling uses the original upstream task
ID. Public download metadata contains artifact keys, while the host fetches CDN
content without forwarding the provider key. Transient polling failures do not
become terminal generation failures. No retail price is hardcoded.

Run the actual Go plugin engine fixtures with:

```sh
go test ./plugins -run TestAPIMartOptionalPluginContract -count=1
```

Image creates require an `Idempotency-Key` header (up to 200 characters).
Use the same key and body to recover a request; use a new key for a new image.

The client entry uses the existing `POST /v1/responses` and
`GET /v1/responses/{id}`. The internal plugin key is not part of a client request.
The user confirmed that no existing image scripts need compatibility. APIMart
is the first provider; configure a stable public model alias on its channel.

Example body (the alias is illustrative and must exist in channel configuration):

```json
{
  "model": "studio-image",
  "background": true,
  "input": "Draw a blue circle on a white background",
  "image": {"size": "1:1", "resolution": "1k"}
}
```

Use Responses `input_image` parts for references. Image background controls live
under `image.background`; top-level `background` is exclusively the async flag.
On completion, the assistant output text contains a JSON object with `images`,
each containing `id` and a host artifact `url`. Provider URLs never appear there.
Only non-streaming sync and background modes are declared. The host pins task
identity for later reads; end-to-end channel switching still needs verification.

The polling host now checkpoints successful `image_generation` results in the
same task row with internal status `PERSISTING` (Responses: `in_progress`). These
tasks are excluded from generation timeout refunds and provider polling. A later
pass resumes image storage from the saved result; partial references are retained.
Only after every image is stored does the host commit success and invoke existing
completion settlement. Downloads require credentialless GET and retain the
existing URL and redirect checks. Stored manifests and content reads do not need
the original provider plugin; restoring still-missing bytes currently needs that
plugin and channel. Missing dependencies leave the task pending, without resubmission.

Enable the byte store with `TASK_ARTIFACT_STORE_MODE=inference`, using the existing
`WILDFLOW_INFERENCE_URL`, `WILDFLOW_INTERNAL_TOKEN` and (when applicable)
`WILDFLOW_INFERENCE_ALLOW_INTERNAL_HTTP`. API must not receive OSS credentials.
Before an image submission, the host writes and reads a tiny fixed PNG in the
same tenant namespace. Checks reuse one object per user. If storage is disabled
or the check fails, submission returns 503 with `Retry-After: 10`, before provider
submission and precharge. A successful check is a point-in-time check; later
storage failures use the saved-completion recovery path above.

Real Beijing OSS verification, the database compatibility matrix, and complete
billing/restart journeys remain release requirements. Tests currently
exercise HTTP download fixtures, storage failures and SQLite task recovery; they
are not evidence of production OSS durability. This candidate alone is not an
onboarded model. It reuses inference artifact ownership and the existing task and
billing ledger.

Sources checked 2026-09-07:

- [Standard image generation](https://docs.apimart.ai/cn/api-reference/images/gpt-image-2/generation)
- [Official channel generation](https://docs.apimart.ai/cn/api-reference/images/gpt-image-2/official)
- [Task status](https://docs.apimart.ai/cn/api-reference/tasks/status)

## Local public-route journey evidence (2026-09-07)

API `953399be` and inference `4edc7d1` ran as separate local processes, with a fresh
SQLite API database and a local PostgreSQL inference database. The channel used
existing APIMart credentials and inference used existing Beijing OSS credentials.
An ordinary user token called `/v1/responses` through public aliases:

| Alias | Upstream model | Result bytes | SHA-256 |
| --- | --- | ---: | --- |
| studio-image-official | gpt-image-2-official | 793132 | 5999a5e7e25a9dfda6d1b64e7eb20ac80fcb78b8526a3e47b647755cce587454 |
| studio-image | gpt-image-2 | 806922 | 11381427075502bedabd7675372dbb77f4f22de0b8074b52b02de95b3f5c0ebc |

Both submissions returned queued response IDs, polling reached completed, and host
artifact capability URLs downloaded PNG bytes matching persisted content digests.
The official result remained retrievable and downloadable after API process restart.
The local `ServerAddress` initially had its default port; correcting it to the actual
candidate origin fixed generated URLs without regenerating the image. Another
ordinary user's token received 404 when retrieving the first user's response.

The fixture used USD 0.01 per task solely to verify the existing ledger, not as a
production retail price. Each task recorded 5000 quota once in both user and token
usage; two consume logs and two task rows existed after retrieval/downloads.
This verifies real APIMart + OSS with local public routes, not a production release.

The image protocol now reserves a user-scoped Operation before provider submission
and atomically attaches its preassigned task ID. Duplicate in-flight POSTs return
202 and the same response ID; changed requests using that key return 409. Completed
replays read the existing result instead of submitting again. An expired unknown
submission returns recovery_required and never grants a new submission attempt.
The public task polling URL also exposes this state before a Task row exists.
Image operations disable automatic retries after invoking a provider. A failed
storage readiness check does not reserve an operation.

A third real local image request exercised first POST (200 queued), immediate same
POST (202 with the same ID), conflicting POST (409), and completed POST replay
(200 completed with the same ID). The replayed PNG had 662132 bytes and SHA-256
09b04bfb89155bf820d469d28c71fd0aea7a734c89aba5df8f4746036c32cbc8.
Across these requests only one Task, one Operation and one 5000-quota charge were
added; total user/token usage changed from 10000 to 15000 and consume logs from
two to three. The subsequent readiness-ordering refinement passed local tests;
the running fixture binary predates that refinement.

Transport-unknown and crash/settlement recovery, provider switching, the full
MySQL/PostgreSQL migration and concurrency matrix, and production release gates
remain open. Happy-path duplicate replay is not proof of every failure path.
