# YouPass – Seamless refresh token (React + TypeScript)

Module xác thực phía FE: access token hết hạn giữa chừng thì tự refresh và chạy tiếp request, người dùng không bị đá về trang login. Thiết kế chi tiết nằm trong tài liệu trả lời (Bài toán 2).

## Chạy test

```bash
npm install
npm test            # vitest: 11 test với backend giả có xoay vòng refresh token + reuse detection
npm run typecheck
```

## Cấu trúc

| File | Vai trò |
|---|---|
| `src/auth/tokenManager.ts` | Giữ access token trong memory; refresh chủ động trước hạn; single-flight; khoá giữa các tab; retry lỗi mạng; trạng thái `session-expired` giữ request chờ đăng nhập lại |
| `src/auth/authFetch.ts` | `fetch` wrapper: gắn Bearer, gặp 401 thì refresh (dùng chung) và replay **1 lần**; clone `Request`; không replay stream; hỗ trợ `AbortSignal`, `minTokenValidityMs` cho upload dài |
| `src/auth/crossTab.ts` | Web Locks API, BroadcastChannel, mốc thời gian refresh chung trong localStorage (không lưu token) |
| `src/auth/AuthProvider.tsx` | React: `useSyncExternalStore`, modal đăng nhập lại hiển thị đè lên trang, hook `useAuthFetch` |
| `src/auth/setup.ts` | Wiring với API YouPass (hợp đồng BE ghi ở đầu file) |
| `src/auth/auth.test.ts` | Test các tình huống: 10 request cùng 401, replay body, refresh chủ động, upload dài, lỗi mạng, phiên hết hạn → đăng nhập lại → chạy tiếp, không lặp vô hạn, abort, 3 tab refresh cùng lúc, đồng bộ login/logout giữa tab |
