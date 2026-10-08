#!/usr/bin/env bash
# Generate host wrappers for PostgreSQL clients inside a running container.
set -euo pipefail
if (($# != 2)); then
  echo "Usage: $0 CONTAINER BIN_DIR" >&2
  exit 2
fi
container=$1
bin_dir=$2
[[ $container =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || { echo 'Invalid PostgreSQL client container name' >&2; exit 2; }
mkdir -p "$bin_dir"
for tool in pg_dump pg_restore psql; do
  cat > "$bin_dir/$tool" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if ((\$# == 1)) && [[ \$1 == --version ]]; then
  exec docker exec -w /rehearsal $(printf '%q' "$container") $tool "\$@"
fi
exec docker exec -i -w /rehearsal -e PGUSER -e PGPASSWORD -e PGDATABASE -e PGSSLMODE -e TZ $(printf '%q' "$container") $tool -h 127.0.0.1 -p 5432 "\$@"
EOF
  chmod 700 "$bin_dir/$tool"
done
