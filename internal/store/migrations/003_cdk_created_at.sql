-- CDK签发时间：list-cdks与Console的cdk_issues对账都需要它（console-fleet-design §11.1）。
-- 已有行没有真实签发时间可考，取迁移执行时刻——这是近似值，不是史实；
-- 003之后新签发的行由DEFAULT写入真实时间。
ALTER TABLE cdks ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
