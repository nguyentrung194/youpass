#!/bin/sh
# End-to-end smoke test against a running API backed by real Postgres and Redis.
set -eu

API=${API:-http://localhost:8080}
BODY=$(mktemp)
UA='Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0)'

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$1"; [ -s "$BODY" ] && printf '    body: %s\n' "$(cat "$BODY")"; exit 1; }

# expect <description> <http status> <curl args...>
expect() {
  desc=$1 want=$2
  shift 2
  got=$(curl -s -o "$BODY" -w '%{http_code}' -A "$UA" "$@")
  [ "$got" = "$want" ] && pass "$desc → $got" || fail "$desc: got $got, want $want"
}

view_count() {
  curl -s -H 'X-User-ID: 1' "$API/api/v1/shares/$CODE" | jq -r .view_count
}

printf 'Waiting for API at %s' "$API"
for _ in $(seq 1 60); do
  curl -sf "$API/healthz" >/dev/null 2>&1 && break
  printf '.'
  sleep 1
done
curl -sf "$API/healthz" >/dev/null || fail "API did not become healthy"
printf '\n\n'

echo "Tạo link"
expect "Chưa đăng nhập bị từ chối" 401 -X POST "$API/api/v1/submissions/1001/share"
expect "Người không sở hữu bài bị từ chối" 403 -X POST -H 'X-User-ID: 2' "$API/api/v1/submissions/1001/share"
expect "Chủ bài tạo link" 201 -X POST -H 'X-User-ID: 1' "$API/api/v1/submissions/1001/share"
CODE=$(jq -r .code "$BODY")
URL=$(jq -r .url "$BODY")
pass "Link: $URL"
expect "Bấm Share lần nữa trả lại đúng link cũ (idempotent)" 200 -X POST -H 'X-User-ID: 1' "$API/api/v1/submissions/1001/share"
[ "$(jq -r .code "$BODY")" = "$CODE" ] && pass "Cùng code $CODE" || fail "code changed"

echo
echo "Xem công khai"
expect "Khách xem được bài làm" 200 "$API/api/v1/s/$CODE"
HEADERS=$(curl -s -D - -o /dev/null -A "$UA" "$API/api/v1/s/$CODE")
echo "$HEADERS" | grep -qi '^cache-control: no-store' && pass "Cache-Control: no-store" || fail "missing no-store"
echo "$HEADERS" | grep -qi '^x-robots-tag: noindex' && pass "X-Robots-Tag: noindex" || fail "missing noindex"
expect "Code sai định dạng" 404 "$API/api/v1/s/not-a-code"
expect "Code không tồn tại" 404 "$API/api/v1/s/zzzzzzzz"

echo
echo "Đếm view"
expect "Crawler Facebook không được đếm" 202 -X POST -A 'facebookexternalhit/1.1' "$API/api/v1/s/$CODE/views"
expect "Prefetch không được đếm" 202 -X POST -H 'Sec-Purpose: prefetch' "$API/api/v1/s/$CODE/views"
expect "Chủ link tự xem không được đếm" 202 -X POST -H 'X-User-ID: 1' "$API/api/v1/s/$CODE/views"
VID=$(curl -s -D - -o /dev/null -X POST -A "$UA" "$API/api/v1/s/$CODE/views" | tr -d '\r' | sed -n 's/^[Ss]et-[Cc]ookie: yp_vid=\([0-9a-f]*\).*/\1/p')
[ -n "$VID" ] && pass "Khách ẩn danh được đếm và nhận cookie yp_vid" || fail "no viewer cookie"
for _ in 1 2 3; do
  curl -s -o /dev/null -X POST -A "$UA" -H "Cookie: yp_vid=$VID" "$API/api/v1/s/$CODE/views"
done
pass "Khách đó F5 thêm 3 lần (phải bị dedupe)"
expect "User 2 xem" 202 -X POST -H 'X-User-ID: 2' "$API/api/v1/s/$CODE/views"
for _ in $(seq 1 20); do [ "$(view_count)" = "2" ] && break; sleep 0.5; done
[ "$(view_count)" = "2" ] && pass "view_count = 2 (1 khách + user 2)" || fail "view_count = $(view_count), want 2"

echo
echo "Tắt / bật / xoá link"
expect "Tắt link" 200 -X PATCH -H 'X-User-ID: 1' -d '{"enabled":false}' "$API/api/v1/shares/$CODE"
expect "Link đã tắt bị chặn ngay lập tức" 404 "$API/api/v1/s/$CODE"
expect "Bật lại" 200 -X PATCH -H 'X-User-ID: 1' -d '{"enabled":true}' "$API/api/v1/shares/$CODE"
expect "URL cũ hoạt động lại" 200 "$API/api/v1/s/$CODE"
expect "Người khác không xoá được (và không biết link tồn tại)" 404 -X DELETE -H 'X-User-ID: 2' "$API/api/v1/shares/$CODE"
expect "Chủ link xoá" 204 -X DELETE -H 'X-User-ID: 1' "$API/api/v1/shares/$CODE"
expect "Link đã xoá bị chặn" 404 "$API/api/v1/s/$CODE"
expect "Không bật lại được link đã xoá" 404 -X PATCH -H 'X-User-ID: 1' -d '{"enabled":true}' "$API/api/v1/shares/$CODE"
expect "Share lại cấp code mới" 201 -X POST -H 'X-User-ID: 1' "$API/api/v1/submissions/1001/share"
[ "$(jq -r .code "$BODY")" != "$CODE" ] && pass "Code mới $(jq -r .code "$BODY") ≠ $CODE" || fail "code was reused"

echo
echo "View được flush vào Postgres"
echo "  (flush chạy mỗi 10s; kiểm tra trực tiếp trong DB)"
if [ -n "${PGHOST:-}" ]; then
  for _ in $(seq 1 30); do
    n=$(psql -tA -c "SELECT view_count FROM share_links WHERE code = '$CODE'")
    [ "$n" = "2" ] && break
    sleep 1
  done
  [ "$n" = "2" ] && pass "share_links.view_count = 2 trong Postgres" || fail "Postgres view_count = $n, want 2"
fi

rm -f "$BODY"
printf '\n\033[32mSmoke test passed.\033[0m\n'
