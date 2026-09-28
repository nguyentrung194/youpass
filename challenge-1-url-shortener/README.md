# YouPass – Share link (URL Shortener) `https://youpass.vn/s/:code`

Go service chia sẻ bài làm qua link rút gọn. Thiết kế chi tiết nằm trong tài liệu trả lời (Bài toán 1); README này mô tả cách chạy và cấu trúc code.

## Chạy

```bash
docker compose up -d                 # Postgres 16 (tự chạy migrations/) + Redis 7
go run ./cmd/server                  # :8080

# Demo (X-User-ID thay cho JWT; bài làm N thuộc user N % 1000)
curl -XPOST localhost:8080/api/v1/submissions/1001/share -H 'X-User-ID: 1'
curl localhost:8080/api/v1/s/<code>
curl -XPOST localhost:8080/api/v1/s/<code>/views -A 'Mozilla/5.0'
curl -XPATCH localhost:8080/api/v1/shares/<code> -H 'X-User-ID: 1' -d '{"enabled":false}'
curl -XDELETE localhost:8080/api/v1/shares/<code> -H 'X-User-ID: 1'
```

## Test

```bash
go test -race ./...                                    # unit test, Redis giả lập bằng miniredis
TEST_DATABASE_URL=postgres://youpass:youpass@localhost:5432/youpass \
  go test -race -run TestPostgresStore ./...           # integration với Postgres thật
```

## Cấu trúc

| File | Vai trò |
|---|---|
| `migrations/001_share_links.sql` | Bảng `share_links` (partial unique index 1 link sống/bài), `share_view_flushes` (idempotency) |
| `internal/sharelink/code.go` | Sinh code base62 8 ký tự từ `crypto/rand` (rejection sampling) |
| `internal/sharelink/service.go` | Tạo (idempotent), bật/tắt, xoá (tombstone), `Resolve` (L1 → L2 → singleflight → DB) |
| `internal/sharelink/cache.go` | L1 in-process TTL 5s, L2 Redis ghi CAS theo `version`, invalidation qua pub/sub |
| `internal/sharelink/views.go` | Đếm view: dedupe `SET NX`, `HINCRBY`, flush batch idempotent, khôi phục batch khi crash |
| `internal/sharelink/http.go` | REST API, lọc bot/prefetch/chủ link, cookie viewer, header `no-store`/`noindex` |
| `internal/sharelink/store_postgres.go` | pgx v5; `UPDATE … FROM unnest()` cho cả batch view |
| `cmd/server/main.go` | Wiring, graceful shutdown (dừng HTTP → drain queue → flush cuối) |

## Test đang bảo vệ

- Tắt link có hiệu lực trên **mọi instance** (L1 của pod khác bị xoá qua pub/sub).
- Một request đọc dữ liệu cũ **không ghi đè** được trạng thái mới hơn trong Redis (CAS theo version).
- 200 request đồng thời vào link chưa có trong cache → **1** query DB (singleflight).
- Code không tồn tại → negative cache; code sai định dạng → 0 I/O.
- Redis sập → resolve vẫn chạy (fallback DB).
- Xoá là vĩnh viễn; chia sẻ lại cấp code mới; link hết hạn → 404.
- View: dedupe theo viewer, bỏ qua bot/prefetch/chủ link, flush không cộng trùng, batch dở dang do crash được xử lý lại đúng 1 lần.
- Postgres thật: tên constraint, partial unique index, 5 flusher đồng thời cùng batch chỉ cộng 1 lần.
