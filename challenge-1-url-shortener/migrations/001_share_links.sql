-- Share links: https://youpass.vn/s/:code -> một bài làm (submission) của học viên.
-- Postgres là source of truth; Redis chỉ là cache + bộ đếm view tạm.

CREATE TABLE IF NOT EXISTS share_links (
    code          varchar(16)  PRIMARY KEY,                -- base62 ngẫu nhiên, không đoán được
    submission_id bigint       NOT NULL,
    owner_id      bigint       NOT NULL,
    status        text         NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'disabled', 'deleted')),
    expires_at    timestamptz,                             -- NULL = không hết hạn
    view_count    bigint       NOT NULL DEFAULT 0,         -- cộng dồn theo batch từ Redis
    version       bigint       NOT NULL DEFAULT 1,         -- tăng mỗi lần đổi trạng thái, chống ghi cache cũ
    created_at    timestamptz  NOT NULL DEFAULT now(),
    updated_at    timestamptz  NOT NULL DEFAULT now()
);

-- Mỗi bài làm chỉ có tối đa 1 link "còn sống" (active/disabled).
-- Link đã xoá giữ lại dạng tombstone để code không bao giờ bị tái sử dụng
-- (URL cũ đã phát tán trên Facebook/Zalo không được trỏ sang nội dung khác).
CREATE UNIQUE INDEX IF NOT EXISTS share_links_submission_live_idx
    ON share_links (submission_id) WHERE status <> 'deleted';

CREATE INDEX IF NOT EXISTS share_links_owner_idx
    ON share_links (owner_id, created_at DESC);

-- Idempotency cho việc flush view từ Redis: mỗi batch chỉ được cộng đúng 1 lần,
-- kể cả khi worker crash giữa chừng và batch được xử lý lại.
-- Job dọn dẹp định kỳ: DELETE FROM share_view_flushes WHERE applied_at < now() - interval '7 days';
CREATE TABLE IF NOT EXISTS share_view_flushes (
    batch_id   text        PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);
