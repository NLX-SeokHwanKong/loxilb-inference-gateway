#!/usr/bin/env bash
# Pin the generator; the published schema stays non-nullable. REST raw-field
# presence distinguishes omission from explicit empty and refuses explicit null.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
target=$(mktemp -d "$root/.uc8-model-generation.XXXXXX")
trap 'rm -rf "$target"' EXIT
printf 'module github.com/loxilb-io/uc8-generation-check\n\ngo 1.26\n' > "$target/go.mod"
if [[ $# == 1 ]]; then
  "$1" generate model --spec api/swagger.yml --name LoadbalanceEntry --target "$target" --model-package models
else
  docker run --rm --user "$(id -u):$(id -g)" -v "$root:$root" -w "$root" quay.io/goswagger/swagger:0.30.3 generate model --spec api/swagger.yml --name LoadbalanceEntry --target "$target" --model-package models
fi
gofmt -w "$target/models/loadbalance_entry.go"
diff -u api/models/loadbalance_entry.go "$target/models/loadbalance_entry.go"
echo 'PASS: LoadbalanceEntry matches go-swagger 0.30.3'
