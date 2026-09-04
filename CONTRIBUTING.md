# Contributing

Internal task planning, priorities, ownership, and progress use self-hosted Multica as the default entry point.
This repository retains source, pull requests, CI, and release evidence; link that evidence from the corresponding
Multica task. Public issues remain available for external feedback and do not require access to internal Multica.

All changes use pull requests against `main`. Preserve upstream attribution and update `UPSTREAM.md` for upstream-derived
changes. For code changes, run the relevant build, test and local boundary commands in `README.md` and record actual
results in the pull request. Documentation-only changes require link and command checks, `git diff --check`, and
`bash scripts/check-local.sh`; they do not require an application build or a full user journey.

Do not commit `.env`, credentials, production data, local databases, generated frontend assets, or tool indexes.
