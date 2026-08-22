#!/usr/bin/env bash
set -euo pipefail

old_dir="${HOME}/.shepherd"
new_dir="${HOME}/.herdsman"

if [[ ! -f "${old_dir}/state.db" ]]; then
  echo "missing source database: ${old_dir}/state.db" >&2
  exit 1
fi

mkdir -p "${new_dir}"
if command -v sqlite3 >/dev/null 2>&1; then
  sqlite3 "${old_dir}/state.db" ".backup '${new_dir}/state.db'"
else
  echo "sqlite3 is required for a consistent database backup" >&2
  exit 1
fi

if [[ -d "${old_dir}/logs" ]]; then
  rm -rf "${new_dir}/logs"
  cp -a "${old_dir}/logs" "${new_dir}/logs"
fi

echo "Migrated state.db and logs from ${old_dir} to ${new_dir}; runtime.json was not copied."
