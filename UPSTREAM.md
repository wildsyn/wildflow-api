# Upstream baseline

- `repository`: https://github.com/QuantumNous/new-api
- `release`: `v1.0.0-rc.24`
- `upstream_baseline`: `5c3abffe8572aa8a49f15c3916707d2019d66af4`
- `excluded_path`: `web/`
- `filtered_history_head`: `77bf43df05b320492979a43cdb094401d509d78d`
- `baseline_tag`: `upstream/v1.0.0-rc.24`
- `imported_at`: `2026-08-17`
- `license`: AGPL-3.0; see `LICENSE` and `NOTICE`

## Reproducible filter

```bash
git clone https://github.com/QuantumNous/new-api.git new-api-api
cd new-api-api
git branch baseline 5c3abffe8572aa8a49f15c3916707d2019d66af4
git filter-repo --force --path web/ --invert-paths --refs refs/heads/baseline
```

The original-to-filtered commit map is:

```text
5c3abffe8572aa8a49f15c3916707d2019d66af4 77bf43df05b320492979a43cdb094401d509d78d
```

`UPSTREAM-README.md` preserves the upstream README from the fixed baseline. The local recovery-only branch
`codex/pre-baseline-snapshot-20260817` records the superseded `e2c7aa…` snapshot and must not be pushed as a product branch.
