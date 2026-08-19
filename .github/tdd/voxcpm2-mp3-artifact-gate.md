# VoxCPM2 MP3 Artifact gate TDD evidence

## Source and user journeys

The journeys were derived during this TDD run; no external plan file was used.

- As a VoxCPM2 caller, I receive one complete `audio/mpeg` result with the
  documented MP3 and completion metadata on initial polling and idempotent replay.
- As a paying caller, incomplete, WAV, or inconsistent TTS output enters
  `recovery_required` and keeps the original reservation instead of settling.
- As an operator, background reconciliation applies the same Artifact gate even
  when the user never polls.
- As a FLUX caller, the existing image Artifact and settlement contract remains
  unchanged.
- As a downloader, an unbounded, oversized, wrong-media-type, or wrong-length
  internal stream is rejected before it is exposed as a valid public MP3.

## RED evidence

Checkpoint `09e5937d` added the controller and reconciler reproducer. The command

```text
go test ./controller ./service -run 'TestSucceededVoxCPM2JobRejectsIncompleteArtifactsAndKeepsReservation|TestWildFlowJobStatusAndArtifactDownloadRemainUserScoped|TestReconcileWildFlowBillingRequiresCompleteVoxMP3Artifact' -count=1
```

failed because all six invalid Vox artifacts returned HTTP 200 and settled, the
reconciler persisted `succeeded`/`settled`, and the public metadata whitelist
omitted the completion fields.

Checkpoint `1be06adc` added the stream-boundary reproducer. The command

```text
go test ./internal/inferenceclient -run 'TestOpenArtifactContentUsesThe320MiBFinalArtifactContract|TestOpenArtifactContentRejectsLengthlessStreamsBeforePublicDownloadStarts' -count=1
```

failed because 200 MiB was rejected by the obsolete 128 MiB cap and a lengthless
chunked response was accepted.

Checkpoint `0e757bec` added recovery lifecycle and slow-stream reproducers. The
targeted tests failed because the normal 30-second request timeout also governed
large response bodies, download header mismatches were not persisted and could
immediately return to `succeeded`, and reserved `recovery_required` operations
were permanently excluded from reconciliation.

Checkpoint `4020a7c3` added the final stream-copy reproducer. A response that
declared the correct bounded length but closed early produced `unexpected EOF`;
the partial HTTP response was unavoidable, but later status and idempotent replay
still incorrectly reported `succeeded`.

## GREEN evidence

Checkpoints `feb1cf3f` and `012dc1cc` implement the Artifact and stream gates.
Checkpoint `7476ad6e` adds public download mismatch coverage. Checkpoint
`d1ec9240` separates bounded streaming from the ordinary request deadline,
persists sticky public recovery state, and lets the background reconciler settle
or refund repaired reserved operations. Checkpoint `a32fed85` persists any body
copy failure as `recovery_required/artifact_stream_error`, so later reads cannot
repeat a false success after an already-started partial download.

| Guarantee | Test target | Type | Result |
|---|---|---|---|
| WAV, missing metadata, partial characters, partial segments, size mismatch, and digest mismatch remain reserved in `recovery_required` | `TestSucceededVoxCPM2JobRejectsIncompleteArtifactsAndKeepsReservation` | controller integration | PASS |
| First polling, idempotent replay, metadata projection, ownership, and MP3 download remain one user-scoped journey | `TestWildFlowJobStatusAndArtifactDownloadRemainUserScoped` | controller integration | PASS |
| Internal media type and length mismatches do not become successful public downloads | `TestDownloadVoxCPM2ArtifactFailsClosedOnInternalContentMismatch` | controller integration | PASS |
| Background reconciliation rejects incomplete Vox completion evidence | `TestReconcileWildFlowBillingRequiresCompleteVoxMP3Artifact` | service integration | PASS |
| Codec, bitrate, sample rate, channels, duration, characters, segment counts, size, and SHA-256 form one settlement gate | `TestValidateWildFlowCompletedArtifactsRequiresCanonicalVoxMP3Evidence` | unit | PASS |
| Existing FLUX image artifacts retain the prior non-empty Artifact rule | `TestValidateWildFlowCompletedArtifactsPreservesFluxArtifactContract` | unit | PASS |
| 320 MiB is the shared final Artifact ceiling and the old 128 MiB boundary is gone | `TestOpenArtifactContentUsesThe320MiBFinalArtifactContract` | client integration | PASS |
| Lengthless streams are rejected before a public response begins | `TestOpenArtifactContentRejectsLengthlessStreamsBeforePublicDownloadStarts` | client integration | PASS |
| A validated bounded body can stream beyond the ordinary inference request timeout | `TestOpenArtifactContentDoesNotApplyRequestTimeoutToAValidatedBodyStream` | client integration | PASS |
| Content mismatches persist `recovery_required`; polling and idempotent replay cannot immediately restore a false success | `TestDownloadVoxCPM2ArtifactFailsClosedOnInternalContentMismatch` | controller integration | PASS |
| Reserved recovery operations are revisited so repaired output settles and terminal provider failure refunds | `TestReconcileWildFlowBillingRevisitsReservedRecoveryOperations` | service integration | PASS |
| An early EOF after correct headers persists stream recovery for subsequent polling and idempotent replay | `TestDownloadVoxCPM2ArtifactPersistsRecoveryAfterStreamFailure` | controller integration | PASS |

Final validation:

```text
make test
go vet ./...
bash scripts/check-local.sh
go test -race ./controller ./service ./internal/inferenceclient -run '<affected VoxCPM2 tests>' -count=1
```

All four commands passed, including the independent `relaykit` module tests,
Docker cross-build contract, attribution checks, and secret-pattern guard.

## Coverage and known gaps

The repository does not define a numeric Go coverage threshold command. The
affected controller, service, reconciler, internal client, download, billing,
and FLUX regression paths are directly exercised, and the full normal test suite
passes.

A diagnostic full-package race run (`go test -race ./controller ./service
./internal/inferenceclient -count=1`) still reports existing unrelated races in
the task-polling/logger test paths and shared quota-cache test state. The targeted
race run for every changed behavior passes; this change does not modify those
unrelated modules.
