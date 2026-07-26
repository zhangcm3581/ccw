-- 账号级额度池的上限。
--
-- 背景：quota.Service一直有完整的双层闸门逻辑（项目级5h/7d + 账号级池双窗口），
-- 但accounts表从来没有存过池上限，worker构造Limits时只能写死一个极大值，
-- 池闸门因此从未生效。多个项目共用一个上游Claude账号时，这是唯一能防止
-- "各自都没超限、加起来把账号打爆"的机制。见用量接线计划§2.4。
--
-- 默认值取得很大（约等于不限制），因为"先记账、后校准"：接线初期只积累数据、
-- 不真正拦人，跑够一周后按真实分布调整。默认值不是推荐值。
--
-- 单位与项目级限额一致，都是"内部额度单位"（由CCW_USAGE_WEIGHTS折算），
-- 不是token数，也不是官方订阅百分比。

ALTER TABLE accounts
  ADD COLUMN pool_five_hour_limit BIGINT NOT NULL DEFAULT 9223372036854775807,
  ADD COLUMN pool_seven_day_limit BIGINT NOT NULL DEFAULT 9223372036854775807;
