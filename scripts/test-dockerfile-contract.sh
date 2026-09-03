#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

grep -Fq 'FROM --platform=$BUILDPLATFORM golang:' Dockerfile
grep -Fq 'ARG TARGETOS' Dockerfile
grep -Fq 'ARG TARGETARCH' Dockerfile
grep -Fq 'ENV GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}' Dockerfile

echo "[wildflow-api] OCI revision label contract"
# The final stage must declare the VCS_REF build arg and stamp it into
# org.opencontainers.image.revision, so images are traceable to an exact git
# SHA via --build-arg VCS_REF=$(git rev-parse HEAD).
grep -Fq 'ARG VCS_REF=untraceable' Dockerfile
grep -Fq 'LABEL org.opencontainers.image.revision="${VCS_REF}"' Dockerfile

# End-to-end proof: build with the exact SHA and read the label back. Skipped
# when Docker is unavailable (contract lines above still run everywhere).
if docker version >/dev/null 2>&1; then
  vcs_ref="$(git rev-parse HEAD)"
  image="wildflow-api-revision-contract:${vcs_ref}"
  docker build --build-arg VCS_REF="${vcs_ref}" -t "${image}" . >/dev/null
  actual="$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.revision"}}' "${image}")"
  if [ "${actual}" != "${vcs_ref}" ]; then
    echo "image revision label mismatch: expected ${vcs_ref}, got ${actual}"
    exit 1
  fi
  echo "image revision label equals build-time git SHA (${vcs_ref})"
else
  echo "docker unavailable; skipped image build verification"
fi

echo "Docker cross-build contract passed"
