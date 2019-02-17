#!/usr/bin/env bash

set -ex

go get -t ./...
if [ ${TESTMODE} == "lint" ]; then 
  curl -L https://git.io/vp6lP | sh
  export PATH="./bin/:$PATH"
  which gometalinter
  gometalinter --debug --deadline=300s ./...
fi

if [ ${TESTMODE} == "unit" ]; then
  ginkgo -r -v -cover -randomizeAllSpecs -randomizeSuites -trace -skipPackage integrationtests,benchmark
fi

if [ ${TESTMODE} == "integration" ]; then
  # run benchmark tests
  ginkgo -randomizeAllSpecs -randomizeSuites -trace benchmark -- -samples=1
  # run benchmark tests with the Go race detector
  # The Go race detector only works on amd64.
  if [ ${TRAVIS_GOARCH} == 'amd64' ]; then
    ginkgo -race -randomizeAllSpecs -randomizeSuites -trace benchmark -- -samples=1 -size=10
  fi
  # run integration tests
  ginkgo -r -v -randomizeAllSpecs -randomizeSuites -trace integrationtests
fi
