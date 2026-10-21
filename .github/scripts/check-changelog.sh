#!/bin/sh
# Fails when a change to the spec or to an expected conformance file comes without an entry in spec/CHANGELOG.md (spec/README.md, Versioning).
# Usage: check-changelog.sh <git range>, for example origin/main...HEAD; run it locally before committing the same way.
set -eu

range=${1:?usage: check-changelog.sh <git range>}
changed=$(git diff --name-only "$range")

needs=$(printf '%s\n' "$changed" |
	grep -E '^spec/|^conformance/(.*/)?expected[^/]*(/|$)|^conformance/errors/' |
	grep -vxE 'spec/CHANGELOG\.md|spec/embed\.go' || true)

if [ -n "$needs" ] && ! printf '%s\n' "$changed" | grep -qx 'spec/CHANGELOG.md'; then
	echo "These files changed without an entry in spec/CHANGELOG.md:"
	printf '%s\n' "$needs" | sed 's/^/  /'
	echo "Add one under the version the change goes into; spec/README.md, Versioning, says whether it is MAJOR, MINOR or PATCH."
	exit 1
fi
