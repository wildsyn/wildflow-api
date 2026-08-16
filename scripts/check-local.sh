#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0

echo "[wildflow-api] license/attribution presence"
test -f LICENSE || { echo "missing LICENSE"; fail=1; }
test -f NOTICE || { echo "missing NOTICE"; fail=1; }
grep -q "AGPL" LICENSE || { echo "LICENSE is not AGPL text"; fail=1; }

echo "[wildflow-api] upstream baseline recorded"
grep -q "5c3abffe8572aa8a49f15c3916707d2019d66af4" UPSTREAM.md || {
  echo "upstream commit not recorded"; fail=1; }

echo "[wildflow-api] frontend split boundary"
test ! -d web || { echo "upstream web/ must live in wildflow-web, not wildflow-api"; fail=1; }

echo "[wildflow-api] secret pattern guard (best effort)"
if grep -RInE "AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----[A-Za-z0-9+/]{20,}|sk-[A-Za-z0-9]{20,}" --exclude-dir=.git --exclude='*.sum' . 2>/dev/null; then
  echo "secret-like pattern found"; fail=1
fi

if [ "$fail" -ne 0 ]; then exit 1; fi
echo "[wildflow-api] local checks passed"
