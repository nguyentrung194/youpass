# YouPass – Bài kiểm tra năng lực, Phần II

| Thư mục | Nội dung |
|---|---|
| [`challenge-1-url-shortener/`](challenge-1-url-shortener/) | Bài toán 1: URL Shortener chia sẻ bài làm (Go, Postgres, Redis) |
| [`challenge-2-seamless-auth/`](challenge-2-seamless-auth/) | Bài toán 2: Refresh token liền mạch ở FE (React + TypeScript) |

## Review bằng 1 lệnh

Chỉ cần Docker, không phải cài Go hay Node:

```bash
./review.sh
```

Script chạy lần lượt:

1. Khởi động Postgres 16 và Redis 7.
2. **Bài 1:** `go test -race`, gồm 13 unit test và 1 integration test trên Postgres thật.
3. **Bài 2:** `tsc --noEmit` và Vitest (11 test).
4. **Bài 1:** build API, chạy trên Postgres và Redis thật, rồi smoke test end-to-end bằng curl. Smoke test kiểm tra: tạo link, xem công khai, header bảo mật, đếm view (lọc bot, lọc prefetch, bỏ qua chủ link, dedupe khi F5), tắt / bật / xoá link, và view đã được flush vào Postgres.

Cuối cùng script in bảng kết quả. API được giữ chạy để bạn thử tay: mặc định ở cổng 8080, nếu cổng bận thì script tự chọn cổng trống kế tiếp.

```bash
./review.sh down     # dọn toàn bộ container và volume
```

Lần chạy đầu mất khoảng 1–2 phút để tải image. Các lần sau mất khoảng 30 giây.

## Chạy không cần Docker

```bash
cd challenge-1-url-shortener && go test -race ./...      # Go 1.26
cd challenge-2-seamless-auth && npm ci && npm test       # Node 20+
```
