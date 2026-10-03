#!/bin/sh
# Mirror the core registry store locally for staging analysis:
#   core:/mnt/data/registry/ -> registry/core-original/
# Uses Homebrew rsync (macOS /usr/bin/rsync is ancient and mangles
# metadata). Exact mirror (--delete): the local copy already holds
# a previous sync, so stales go. The guard below refuses to run
# when the remote side lists empty — a wrong path must never wipe
# the local copy. Never run against the live registry volume —
# this target is analysis-only.
set -eu

RSYNC=/opt/homebrew/bin/rsync
REMOTE=core:/mnt/data/registry/
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DEST="$ROOT/registry/core-original/"

if [ ! -x "$RSYNC" ]; then
	echo "missing $RSYNC (brew install rsync)" >&2
	exit 1
fi
mkdir -p "$DEST"

if [ -z "$("$RSYNC" --list-only "$REMOTE" 2>/dev/null)" ]; then
	echo "remote $REMOTE lists empty: refusing to mirror (would wipe $DEST)" >&2
	exit 1
fi

exec "$RSYNC" -avz --delete --partial --progress "$REMOTE" "$DEST"
