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

The internal plugin key is not the client contract. A provider-independent submit
entry, durable recovery and Beijing OSS persistence must be completed before
release. The upstream artifact store is currently a disabled placeholder; its
proxy URL does not prove permanent storage. Reuse the inference repository's
artifact ownership and existing OSS configuration, rather than creating a second
independent task or billing ledger. This candidate alone is not an onboarded model.

Sources checked 2026-09-07:

- [Standard image generation](https://docs.apimart.ai/cn/api-reference/images/gpt-image-2/generation)
- [Official channel generation](https://docs.apimart.ai/cn/api-reference/images/gpt-image-2/official)
- [Task status](https://docs.apimart.ai/cn/api-reference/tasks/status)
