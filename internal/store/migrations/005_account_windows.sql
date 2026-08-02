-- 账号窗口边界（2026-08-02）。
--
-- 此前项目额度用的是**滚动窗口**（now-5h / now-7d），而 Claude 账号是
-- **固定窗口**：到 resets_at 那一刻归零。两种模型不一致会带来两个问题：
--
--   1. Claude 重置了，项目额度却还背着重置前的用量，要等它慢慢滑出去；
--   2. 校准拿"我们的滚动累计 ÷ Claude 的固定窗口百分比"，两边量的不是
--      同一段时间，跨过重置点时比值会失真。
--
-- 现在把 Claude 报的 resets_at 存下来，两边用同一个窗口边界。
-- 值来自状态行回传的账号快照（唯一能拿到真实 rate_limits 的入口）。
-- NULL 表示还没拿到过——那时退回滚动窗口，行为与改动前一致。

ALTER TABLE accounts
  ADD COLUMN five_hour_resets_at TIMESTAMPTZ,
  ADD COLUMN seven_day_resets_at TIMESTAMPTZ;
