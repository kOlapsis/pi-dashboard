#!/bin/sh
set -eu

offenders=$(go list -deps -f '{{if .CgoFiles}}{{.ImportPath}}{{end}}' ./cmd/pi-dashboard \
	| grep -E '^[a-z0-9-]+\.[a-z]+/' || true)

if [ -n "$offenders" ]; then
	echo "CGO dependencies found (the binary must cross-compile with CGO_ENABLED=0):" >&2
	echo "$offenders" | sed 's/^/  /' >&2
	exit 1
fi

echo "CGO: none."
