#!/usr/bin/env bash
# Offline regression for the PostgreSQL client wrappers used by test-utf8-migration.sh.
set -euo pipefail
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-pg-wrappers.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/fake-bin"
cat > "$work/fake-bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\0' "$@" > "$CAPTURE_ARGS"
for arg in "$@"; do
  if [[ $arg == -i ]]; then cat > "$CAPTURE_STDIN"; exit; fi
done
: > "$CAPTURE_STDIN"
DOCKER
chmod 700 "$work/fake-bin/docker"
bash scripts/create-postgres-client-wrappers.sh utf8-wrapper-test "$work/bin"
CAPTURE_ARGS="$work/args"
CAPTURE_STDIN="$work/stdin"
export CAPTURE_ARGS CAPTURE_STDIN

assert_args() {
  local expected_file=$1
  shift
  printf '%s\0' "$@" > "$expected_file"
  cmp "$expected_file" "$CAPTURE_ARGS"
}
for tool in pg_dump pg_restore psql; do
  # Expand these variables only inside the child shell.
  # shellcheck disable=SC2016
  probe=$(printf 'RESTORE\n' | env PATH="$work/fake-bin:$PATH" WRAPPER="$work/bin/$tool" bash -c '
    "$WRAPPER" --version
    IFS= read -r confirmation
    printf "|%s" "$confirmation"
  ')
  [[ $probe == '|RESTORE' ]]
  assert_args "$work/expected" exec -w /rehearsal utf8-wrapper-test "$tool" --version
  [[ ! -s $work/stdin ]]

  printf 'restore-stream' | env PATH="$work/fake-bin:$PATH" \
    PGUSER=operator PGPASSWORD=secret PGDATABASE=target PGSSLMODE=disable TZ=UTC \
    "$work/bin/$tool" '--dbname=target database' --exit-on-error
  assert_args "$work/expected" exec -i -w /rehearsal -e PGUSER -e PGPASSWORD -e PGDATABASE -e PGSSLMODE -e TZ \
    utf8-wrapper-test "$tool" -h 127.0.0.1 -p 5432 '--dbname=target database' --exit-on-error
  [[ $(cat "$work/stdin") == restore-stream ]]
done
echo 'PostgreSQL client wrapper regressions passed (version probes omit connection flags and preserve confirmation stdin; normal arguments, environment and stdin are preserved).'
