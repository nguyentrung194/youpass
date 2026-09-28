#!/usr/bin/env bash
# One command for reviewers: runs every test for both challenges, then starts
# the share-link API and exercises it end to end. Requires only Docker.
#
#   ./review.sh          test everything, leave the API running on :8080
#   ./review.sh down     stop and remove everything
set -euo pipefail
cd "$(dirname "$0")"

compose() { docker compose "$@"; }

if [[ "${1:-}" == "down" ]]; then
  compose --profile test down -v --remove-orphans
  exit 0
fi

command -v docker >/dev/null || { echo "Docker is required: https://docs.docker.com/get-docker/"; exit 1; }

step() { printf '\n\033[1;36m━━ %s\033[0m\n' "$1"; }
results=()
run() {
  local name=$1; shift
  if "$@"; then
    results+=("\033[32m✓\033[0m $name")
  else
    results+=("\033[31m✗\033[0m $name")
    summary
    exit 1
  fi
}
summary() {
  step "Kết quả"
  for r in "${results[@]}"; do printf "  $r\n"; done
}

# Use API_PORT if given, otherwise the first free port from 8080.
port_busy() { (echo >"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
if [[ -z "${API_PORT:-}" ]]; then
  API_PORT=8080
  while port_busy "$API_PORT"; do API_PORT=$((API_PORT + 1)); done
fi
export API_PORT

step "1/4  Khởi động Postgres 16 + Redis 7"
run "Hạ tầng (Postgres, Redis)" compose up -d --wait postgres redis

step "2/4  Bài 1 – Go: unit test (-race) + integration test với Postgres thật"
run "Bài 1: go test -race (14 test)" compose run --rm backend-test

step "3/4  Bài 2 – TypeScript: typecheck + Vitest"
run "Bài 2: typecheck + vitest (11 test)" compose run --rm frontend-test

step "4/4  Bài 1 – chạy API thật và smoke test end-to-end (Postgres + Redis thật)"
run "Build & start API" compose up -d --build --wait api
run "Bài 1: smoke test end-to-end" compose run --rm smoke

summary
port=$API_PORT
cat <<EOF

API đang chạy tại http://localhost:$port  (X-User-ID thay cho JWT; bài làm N thuộc user N % 1000)

  curl -XPOST localhost:$port/api/v1/submissions/2001/share -H 'X-User-ID: 1'
  curl localhost:$port/api/v1/s/<code>
  curl -XPATCH localhost:$port/api/v1/shares/<code> -H 'X-User-ID: 1' -d '{"enabled":false}'

Dọn dẹp:  ./review.sh down
EOF
