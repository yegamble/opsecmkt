#!/usr/bin/env bash

# Extract a Git source archive with its tracked permissions intact. The upgrade
# rehearsal runs with umask 077 for its private environment and backup files,
# but the unprivileged app image still needs the source tree's public read/execute
# bits after Docker copies it into the container.
# Git archive uses the selected commit's export-ignore rules. Apply the release
# exclusions here too for historical refs that predate .gitattributes, without
# using --worktree-attributes to change the meaning of a committed archive.
extract_a144_source() {
  local archive=$1 destination=$2
  tar -xpf "$archive" -C "$destination" \
    --exclude='.claude' --exclude='.ralph' --exclude='.ralphrc' \
    --exclude='design-reference' --exclude='.wrangler' \
    --exclude='tests/test_claude_review_hook.py'
}
