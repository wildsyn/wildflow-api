# Upstream baseline

- `repository`: https://github.com/QuantumNous/new-api
- `release`: `v1.0.0-rc.34`
- `upstream_baseline`: `0c76e4dae77a279e015329b7478e6f02d6b62edd`
- `excluded_path`: `web/`
- `filtered_history_head`: `a051ae58b43d13e44f68235d79fbe9cbf2f99672`
- `initial_baseline_tag`: `upstream/v1.0.0-rc.24`
- `candidate_status`: isolated paired evaluation; not approved for production
- `imported_at`: `2026-09-07`
- `license`: AGPL-3.0; see `LICENSE` and `NOTICE`

## Reproducible filter

```bash
git clone https://github.com/QuantumNous/new-api.git new-api-api
cd new-api-api
git branch baseline 0c76e4dae77a279e015329b7478e6f02d6b62edd
git filter-repo --force --path web/ --invert-paths --refs refs/heads/baseline
```

The original-to-filtered commit map is:

```text
0c76e4dae77a279e015329b7478e6f02d6b62edd a051ae58b43d13e44f68235d79fbe9cbf2f99672
```

`UPSTREAM-README.md` preserves the upstream README from the fixed baseline. The local recovery-only branch
`codex/pre-baseline-snapshot-20260817` records the superseded `e2c7aa…` snapshot and must not be pushed as a product branch.

## Previous import

The initial rc.24 import used `5c3abffe8572aa8a49f15c3916707d2019d66af4` and filtered head `77bf43df05b320492979a43cdb094401d509d78d`.
The rc.34 candidate is merged from full filtered history, preserving that ancestry.
Both WildFlow repositories must be reviewed together. Upstream rc.34 release notes
advise against production use; passing local gates does not change that warning.
