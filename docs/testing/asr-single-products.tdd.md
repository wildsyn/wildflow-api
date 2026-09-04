# Single-engine ASR products — TDD evidence

Approved 2026-09-04: Whisper CNY 0.02/audio minute, VibeVoice CNY 0.04/audio minute,
existing dual CNY 0.05/audio minute unchanged. This adds product registrations, not
a daily repricing mechanism or a second ledger.

- RED `a84aa71c`: `go test ./service -run '^TestASRProductsPriceAndTrustedEngineSelection$' -count=1`
  failed for both new IDs with unsupported model.
- GREEN `7d38936b`: same test passed; related service/controller regressions passed.
- Additional tests cover 2.4/4.8/6 CNY reservation, exact millisecond settlement,
  missing/wrong engine artifact rejection, client selector injection, idempotent
  submission/reservation and the shared 14-day queue deadline.
- `go test ./service ./controller ./model -run 'ASR|WildFlow' -count=1`: passed.
- No schema, wallet, subscription algorithm, TTS or Qwen changes.
- Live deployment, three-model public journey and billing verification are separate
  release gates; this file does not claim them passed.
