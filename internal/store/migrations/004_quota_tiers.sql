-- 额度档位（2026-08-02）。
--
-- 项目的限额不再是一个写死的绝对值，而是「档位百分比 × 账号池上限」推导出来的。
-- 池上限由真实账号用量自动校准（internal/quota/calibrate.go），所以百分比
-- 才真的对应现实中的那一份。
--
-- 百分比存成万分之一（basis points）而不是浮点：限额是要拿去做比较的整数，
-- 用浮点会引入"到底算不算超"的边界抖动。1000 bp = 10%。

CREATE TABLE quota_tiers (
  name TEXT PRIMARY KEY,          -- 2x / 5x / 7x
  share_bp INT NOT NULL,          -- 占账号池的比例，万分之一
  sort_order INT NOT NULL DEFAULT 0,
  CHECK (share_bp > 0 AND share_bp <= 10000)
);

INSERT INTO quota_tiers (name, share_bp, sort_order) VALUES
  ('2x', 1000, 1),   -- 10%
  ('5x', 2500, 2),   -- 25%
  ('7x', 3300, 3);   -- 33%

-- 项目挂一个档位。NULL 表示沿用 five_hour_limit/seven_day_limit 里的绝对值
-- ——已有部署不会因为这次迁移突然换一套限额。
ALTER TABLE projects ADD COLUMN tier TEXT REFERENCES quota_tiers(name);
