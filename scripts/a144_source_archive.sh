#!/usr/bin/env bash

# Extract a Git source archive with its tracked permissions intact. The upgrade
# rehearsal runs with umask 077 for its private environment and backup files,
# but the unprivileged app image still needs the source tree's public read/execute
# bits after Docker copies it into the container.
extract_a144_source() {
  local archive=$1 destination=$2
  tar -xpf "$archive" -C "$destination"
}
