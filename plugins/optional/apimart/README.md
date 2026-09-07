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

The inference-backed store is implemented but not yet enabled at startup. Storage
admission checks, real Beijing OSS verification, the database compatibility matrix,
and complete billing/restart journeys remain release requirements. Tests currently
exercise HTTP download fixtures, storage failures and SQLite task recovery; they
are not evidence of production OSS durability. This candidate alone is not an
onboarded model. It reuses inference artifact ownership and the existing task and
billing ledger.

Sources checked 2026-09-07:

- [Standard image generation](https://docs.apimart.ai/cn/api-reference/images/gpt-image-2/generation)
- [Official channel generation](https://docs.apimart.ai/cn/api-reference/images/gpt-image-2/official)
- [Task status](https://docs.apimart.ai/cn/api-reference/tasks/status)
