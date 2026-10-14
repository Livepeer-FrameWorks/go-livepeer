#!/usr/bin/env bash

set -eux

# Install gotestsum for flaky test retry support
command -v gotestsum >/dev/null 2>&1 || go install gotest.tools/gotestsum@latest

RERUN="--rerun-fails --rerun-fails-max-failures=3 --rerun-fails-run-root-test"

mkdir -p test-results

# Test script to run all the tests except of e2e tests for continuous integration
PKGS=$(go list ./... | grep -v 'test/e2e')
gotestsum ${RERUN} --junitfile test-results/unit.xml \
  --packages="${PKGS}" -- -coverprofile cover.out

# Whole-package race lanes run without reruns: a race that a retry hides is
# still a race.
for pkg in core server discovery media; do
  gotestsum --junitfile "test-results/${pkg}-race.xml" \
    --packages="./${pkg}" -- -race -count=1
done

# Be more strict with live runner tests: run with race detector enabled
go test ./ai/runner -race

gotestsum ${RERUN} --junitfile test-results/trickle-race.xml \
  --packages="./trickle" -- -race -timeout 10s

gotestsum ${RERUN} --junitfile test-results/byoc-race.xml \
  --packages="./byoc" -- -race -timeout 10s

./test_args.sh

printf "\n\nAll Tests Passed\n\n"
