# Phase 1证据：Claude Code JSONL用量语义

**日期：** 2026-07-19
**任务：**实施计划Task 0 Step 3（架构阻断验证，零成本，本机现成数据）
**数据源：**本机Claude Code会话记录（只读扫描，未修改）

## 扫描规模

- 扫描文件数：62
- 总行数：8809
- `type==assistant`且含`message.usage`的记录数：3279
- 坏JSON行：0（真实数据无坏行；脱敏样例中人为加入一条测试容错）

## 核心发现：同一requestId多条记录的计量语义

| 指标 | 数值 |
|---|---|
| 不同requestId数 | 1380 |
| 出现多条记录的requestId数 | 1053（占76%） |
| 多条记录中四类token字段逐条完全相同的 | 1053/1053（100%） |
| 多条记录中"最后一条==各字段最大值"的 | 1053/1053（100%） |

**结论：**同一requestId出现多条usage记录是常态（76%），但这些记录的`input_tokens`、`output_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens`四个字段**逐条完全相同**。因此"取最终记录"与"取各字段最大值"在真实数据上**完全等价**。

**采纳语义：**采集器Sink对`(project_id, source_event_id)`冲突时用`GREATEST`各字段取最大值更新（实施计划Task 8）。理由：

1. 与真实数据一致（多条值相同，GREATEST结果不变）；
2. 幂等——多条同requestId只upsert到同一行，**绝不重复计数**；
3. 对未来可能出现的"中间记录偏小、最终记录偏大"情形稳健，不会永久少计。

## 其他异常形态

- **无requestId但含usage的行：** 14条（占3279的0.4%）。采集器按`requestId==""`跳过，不计入用量、不算坏行。占比极小，对额度统计影响可忽略；已在语义文档留痕，若未来占比上升需重新评估。
- **超长行：**未发现超过扫描缓冲（8MB）的行；采集器仍保留超长行计数指标以防未来出现。
- **半行/截断：**本次静态扫描未涉及；采集器的`committed_offset`+`partial_line`机制在Task 8单元测试中覆盖。

## 脱敏说明

样例文件`internal/usage/testdata/session-sample.jsonl`仅保留采集器实际解析的结构字段（`type`、`requestId`、`timestamp`、`message.model`、`message.usage`四个token字段）。真实记录中的`cwd`、`gitBranch`、`sessionId`、`message.content`（对话正文）、工具参数等敏感字段一律未提取、未写入。requestId改写为`req_S1`/`req_S2`/`req_S3`代号，token数值取自真实记录量级但已与具体会话解绑。

样例覆盖场景：单条usage、同requestId两条（token相同，反映76%常态）、user行（跳过）、坏JSON行（计入坏行）、无requestId的usage行（跳过）、多模型（fable/opus）。对应Task 8的`ParseLines`应得4个event、1个坏行。
