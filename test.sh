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

# Be more strict with load balancer tests: run with race detector enabled
gotestsum ${RERUN} --junitfile test-results/core-lb-race.xml \
  --packages="./core" -- -race -run 'LB_'

# Be more strict with nvidia tests: run with race detector enabled
gotestsum ${RERUN} --junitfile test-results/core-nvidia-race.xml \
  --packages="./core" -- -race -run 'Nvidia_'

gotestsum ${RERUN} --junitfile test-results/core-capabilities-race.xml \
  --packages="./core" -- -race -run 'Capabilities_'

# Be more strict with discovery tests: run with race detector enabled
gotestsum ${RERUN} --junitfile test-results/discovery-race.xml \
  --packages="./discovery" -- -race

# Be more strict with HTTP push tests: run with race detector enabled
gotestsum ${RERUN} --junitfile test-results/server-selectsession-race.xml \
  --packages="./server" -- -race -run 'TestSelectSession_'

gotestsum ${RERUN} --junitfile test-results/server-registerconnection-race.xml \
  --packages="./server" -- -race -run 'RegisterConnection'

gotestsum ${RERUN} --junitfile test-results/media-race.xml \
  --packages="./media" -- -race

gotestsum ${RERUN} --junitfile test-results/trickle-race.xml \
  --packages="./trickle" -- -race -timeout 10s

gotestsum ${RERUN} --junitfile test-results/byoc-race.xml \
  --packages="./byoc" -- -race -timeout 10s

./test_args.sh

printf "\n\nAll Tests Passed\n\n"
