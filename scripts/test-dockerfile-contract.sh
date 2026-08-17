#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

grep -Fq 'FROM --platform=$BUILDPLATFORM golang:' Dockerfile
grep -Fq 'ARG TARGETOS' Dockerfile
grep -Fq 'ARG TARGETARCH' Dockerfile
grep -Fq 'ENV GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}' Dockerfile

echo "Docker cross-build contract passed"
