# 存储设计

## Store 契约

`Store` 以访问器暴露子端口，而非内嵌它们：

```go
type Store interface {
    Messages()   MessageStore
    Deliveries() DeliveryStore
    Attempts()   AttemptStore
    Templates()  TemplateStore
    Inbox()      InboxStore
    Receipts()   ReceiptStore
    Ping(ctx) error
    Close() error
}
```

调用方只依赖其需要的最窄端口：inbox handler 取 `InboxStore`，重试循环取 `DeliveryStore`。这使得测试可以构造实现四个方法的 stub，而非实现四十个方法的 fake store。

三个方法组仅因"store 是真相来源"而存在：

| 方法 | 作用 | 缺失后果 |
|---|---|---|
| `ClaimDue` / `Release` | 带租约取得投递所有权，并能不消耗重试次数地归还 | 无法区分"尚未尝试"与"尝试后失败" |
| `ReclaimExpiredLeases` | 恢复被已退出进程持有的投递 | 每次崩溃永久搁浅该进程正在处理的投递 |
| `ListDueScheduled` | 找出停机期间到点的定时消息 | 每个发布窗口内到点的定时消息丢失 |

## 关键方法的契约

### `Deliveries().ClaimDue`

系统中最热的方法。四项约束必须同时成立：

1. **原子性**。两个并发 worker 不得取得同一条投递。先 `SELECT` 再 `UPDATE` 在压力下会将同一行分发给两者，现象是仅在流量高峰出现的重复消息。
2. **同时设置** `Status`、`LeaseOwner`、`LeaseExpiresAt`、`NextAttemptAt`。设置了状态而未设置租约会产生永远无法回收的投递。
3. **不得递增 `AttemptCount`**。计数只由 `CommitAttempt` 递增。在 claim 阶段递增会为可能不发生的操作扣除重试预算，使滚动发布演变为大规模投递失败。
4. **按 `NextAttemptAt <= now` 选行**，按优先级、再按到期时间排序。

### `Deliveries().Release`

用于 worker 正在关闭，或尝试在触达服务商之前被中止。清除租约，状态回到 `pending`，`AttemptCount` 不变。

误用该方法是本接口中影响最大的缺陷：每次优雅关闭都会从每条在途投递扣除一次重试，频繁重启的部署会耗尽从未被尝试过的消息的重试预算。

### `Deliveries().CommitAttempt`

唯一递增计数的位置。处理三种结果：成功置为 `accepted`；可重试且有余量置为 `retrying` 并写入调用方计算的退避时间；永久失败或余量耗尽置为 `failed`。

退避由 dispatcher 计算，不由 store 计算。它依赖按通道、按部署而变的策略，不属于持久化层。

必须同时更新父消息状态，或在同一事务内由调用方更新。全部投递已结束而消息状态仍为 `pending` 时，客户端会持续轮询。

### `Messages().Create`

必须通过捕获 INSERT 的唯一键冲突发现重复，不得先 `SELECT` 再 `INSERT`。后者在并发相同提交下会竞态：两个客户端同时重试同一请求，双方均认为不存在已存行，双方均插入。这正是幂等机制需要覆盖的场景。

### `Inbox().MarkRead`

必须限定 `userID`，否则任何调用方都能标记任何用户的消息。store 是能拦截该问题的最后一层。

必须幂等：客户端在超时后重试时，不应因第一次已成功而收到错误。

## MySQL 表结构

目标 MySQL 8.0+，`utf8mb4`，`InnoDB`。

### 约定

| 约定 | 理由 |
|---|---|
| ULID 存为 `BINARY(16)` | 索引体积为 `CHAR(26)` 的一半。ULID 的字典序即时间序，主键因此插入有序，避免 UUIDv4 在 InnoDB 上造成的页分裂 |
| 每张表包含 `tenant_id` 且置于复合索引前缀 | 给在线复合索引补充租户维度需要重建表 |
| `next_attempt_at` 使用 `NOT NULL` 加哨兵值 `9999-12-31` | `NULL` 在索引中排最前，会破坏 claim 索引的范围扫描 |
| 时间使用 `DATETIME(3)` | 毫秒精度足够，且不受会话时区变量影响 |

### messages

```sql
CREATE TABLE messages (
  id                BINARY(16)   NOT NULL,
  tenant_id         VARCHAR(64)  NOT NULL DEFAULT '',
  idempotency_key   VARCHAR(128) NULL,
  biz_type          VARCHAR(64)  NOT NULL,
  channels          VARCHAR(255) NOT NULL,
  recipient_user_id VARCHAR(64)  NOT NULL,
  recipient         JSON         NOT NULL,
  content           JSON         NOT NULL,
  channel_content   JSON         NULL,
  priority          VARCHAR(16)  NOT NULL DEFAULT 'normal',
  status            VARCHAR(24)  NOT NULL,
  scheduled_at      DATETIME(3)  NULL,
  expires_at        DATETIME(3)  NULL,
  max_attempts      INT          NOT NULL DEFAULT 0,
  metadata          JSON         NULL,
  created_at        DATETIME(3)  NOT NULL,
  updated_at        DATETIME(3)  NOT NULL,
  completed_at      DATETIME(3)  NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_messages_idem (tenant_id, idempotency_key),
  KEY idx_messages_user_created (tenant_id, recipient_user_id, created_at DESC, id DESC),
  KEY idx_messages_sched (status, scheduled_at),
  KEY idx_messages_biz_created (tenant_id, biz_type, created_at DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

| 索引 | 服务的访问模式 |
|---|---|
| `uk_messages_idem` | 幂等去重。`idempotency_key` 可为 NULL，MySQL 唯一索引允许多个 NULL，恰好使不带幂等键的消息不受约束 |
| `idx_messages_user_created` | 按用户查询消息列表，keyset 分页 |
| `idx_messages_sched` | Scheduler 的 `ListDueScheduled` |
| `idx_messages_biz_created` | 按事件类型查询 |

`uk_messages_idem` 意味着幂等键按租户永久唯一。若需要 TTL，需要一张带过期时间的独立 `idempotency_keys` 表；替代方案是 sweeper 将旧键置 NULL，但那会破坏"同一请求永远重放同一条消息"的保证。上线前需要决定。

### deliveries

```sql
CREATE TABLE deliveries (
  id                BINARY(16)   NOT NULL,
  message_id        BINARY(16)   NOT NULL,
  tenant_id         VARCHAR(64)  NOT NULL DEFAULT '',
  channel           VARCHAR(32)  NOT NULL,
  recipient         JSON         NOT NULL,
  address_form      VARCHAR(32)  NOT NULL DEFAULT '',
  address_value     VARCHAR(512) NOT NULL DEFAULT '',
  content           JSON         NOT NULL,
  priority          VARCHAR(16)  NOT NULL DEFAULT 'normal',
  status            VARCHAR(24)  NOT NULL,

  attempt_count     INT          NOT NULL DEFAULT 0,
  max_attempts      INT          NOT NULL,
  next_attempt_at   DATETIME(3)  NOT NULL,
  lease_owner       VARCHAR(128) NOT NULL DEFAULT '',
  lease_expires_at  DATETIME(3)  NULL,
  expires_at        DATETIME(3)  NULL,

  failure           JSON         NULL,
  milestone         VARCHAR(16)  NOT NULL DEFAULT 'none',
  bounced           VARCHAR(16)  NOT NULL DEFAULT '',
  bounced_at        DATETIME(3)  NULL,
  complained_at     DATETIME(3)  NULL,
  last_receipt_at   DATETIME(3)  NULL,
  read_at           DATETIME(3)  NULL,

  created_at        DATETIME(3)  NOT NULL,
  updated_at        DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_deliveries_claim (tenant_id, channel, status, next_attempt_at, priority),
  KEY idx_deliveries_message (message_id),
  KEY idx_deliveries_lease (status, lease_expires_at),
  KEY idx_deliveries_inbox (tenant_id, recipient_user_id, created_at DESC, id DESC),
  KEY idx_deliveries_unread (tenant_id, recipient_user_id, read_at),
  KEY idx_deliveries_funnel (tenant_id, channel, milestone, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

| 索引 | 服务的访问模式 |
|---|---|
| `idx_deliveries_claim` | `ClaimDue` 的热路径。列序使范围扫描落在 `next_attempt_at`、排序落在 `priority`。`next_attempt_at` 必须非 NULL，否则该范围扫描退化为全表扫描 |
| `idx_deliveries_lease` | `ReclaimExpiredLeases`，只扫描 `sending` 状态行 |
| `idx_deliveries_inbox` | 站内信收件箱列表，keyset 分页 |
| `idx_deliveries_unread` | 未读数，覆盖索引 |
| `idx_deliveries_funnel` | 按通道的漏斗报表，读取派生的 `milestone` |

`deliveries` 同时承担待办工作项与站内信收件箱行两种角色。`recipient_user_id` 从 `recipient` JSON 中提升为实列，使上述索引无需依赖 JSON 函数索引。

### delivery_attempts

```sql
CREATE TABLE delivery_attempts (
  id                  BINARY(16)  NOT NULL,
  delivery_id         BINARY(16)  NOT NULL,
  message_id          BINARY(16)  NOT NULL,
  tenant_id           VARCHAR(64) NOT NULL DEFAULT '',
  channel             VARCHAR(32) NOT NULL,
  attempt_no          INT         NOT NULL,
  status              VARCHAR(16) NOT NULL,
  started_at          DATETIME(3) NOT NULL,
  finished_at         DATETIME(3) NULL,
  duration_ns         BIGINT      NULL,
  failure             JSON        NULL,
  provider_message_id VARCHAR(255) NOT NULL DEFAULT '',
  response_digest     CHAR(64)    NOT NULL DEFAULT '',
  response_snippet    VARCHAR(512) NOT NULL DEFAULT '',
  worker_id           VARCHAR(128) NOT NULL DEFAULT '',
  PRIMARY KEY (id, started_at),
  UNIQUE KEY uk_attempt_delivery_no (delivery_id, attempt_no),
  KEY idx_attempts_message (message_id, started_at DESC),
  KEY idx_attempts_orphan (status, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
PARTITION BY RANGE COLUMNS(started_at) (
  PARTITION p2026q1 VALUES LESS THAN ('2026-04-01'),
  PARTITION p2026q2 VALUES LESS THAN ('2026-07-01'),
  PARTITION p2026q3 VALUES LESS THAN ('2026-10-01'),
  PARTITION pmax    VALUES LESS THAN (MAXVALUE)
);
```

只追加：行在 in-flight 时插入，一次定稿。被崩溃孤儿化的尝试保留原样，不清理。

`uk_attempt_delivery_no` 提供幂等保护：同一投递的同一尝试号写入两次属于缺陷，不应静默产生两行。

`idx_attempts_orphan` 供对账任务找出孤儿尝试（`status = 'in_flight'` 且已超时）。孤儿尝试的数量是"服务商可能已接收但未记账"的度量。

分区不解决单表插入热点。该表在数月内会达到一亿行量级，需要归档路径或迁移到只追加日志。任何界面中都不应对其执行 `COUNT(*)`。

### delivery_receipts

```sql
CREATE TABLE delivery_receipts (
  id                BINARY(16)    NOT NULL,
  delivery_id       BINARY(16)    NOT NULL,
  message_id        BINARY(16)    NOT NULL,
  tenant_id         VARCHAR(64)   NOT NULL DEFAULT '',
  channel           VARCHAR(32)   NOT NULL,
  kind              VARCHAR(24)   NOT NULL,
  occurred_at       DATETIME(3)   NOT NULL,
  received_at       DATETIME(3)   NOT NULL,
  bounce_type       VARCHAR(16)   NOT NULL DEFAULT '',
  url               VARCHAR(2048) NOT NULL DEFAULT '',
  user_agent        VARCHAR(512)  NOT NULL DEFAULT '',
  ip                VARBINARY(16) NULL,
  provider_event_id VARCHAR(128)  NOT NULL DEFAULT '',
  provider          VARCHAR(64)   NOT NULL DEFAULT '',
  raw               VARCHAR(1024) NOT NULL DEFAULT '',
  PRIMARY KEY (id),
  UNIQUE KEY uk_receipt_dedup (delivery_id, provider_event_id, kind, occurred_at),
  KEY idx_receipts_message (message_id, occurred_at DESC),
  KEY idx_receipts_delivery (delivery_id, occurred_at DESC),
  KEY idx_receipts_lag (received_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
PARTITION BY RANGE COLUMNS(occurred_at) (
  PARTITION p2026q1 VALUES LESS THAN ('2026-04-01'),
  PARTITION p2026q2 VALUES LESS THAN ('2026-07-01'),
  PARTITION pmax    VALUES LESS THAN (MAXVALUE)
);
```

| 索引 | 服务的访问模式 |
|---|---|
| `uk_receipt_dedup` | 幂等去重。`provider_event_id` 为空时退化为 `(delivery, kind, occurred_at)` 自然键 |
| `idx_receipts_message` | 单条消息的完整漏斗回溯 |
| `idx_receipts_lag` | 回调延迟监控，即 `received_at - occurred_at` 的分布 |

`ip` 使用 `VARBINARY(16)` 以容纳 IPv4 与 IPv6。该列为 PII，投产前需要哈希或丢弃。

### templates

```sql
CREATE TABLE templates (
  id                 BINARY(16)  NOT NULL,
  tenant_id          VARCHAR(64) NOT NULL DEFAULT '',
  biz_type           VARCHAR(64) NOT NULL,
  channel            VARCHAR(32) NOT NULL DEFAULT '',
  locale             VARCHAR(16) NOT NULL DEFAULT '',
  version            INT         NOT NULL,
  subject_template   TEXT        NULL,
  body_template      TEXT        NOT NULL,
  body_format        VARCHAR(16) NOT NULL DEFAULT 'text',
  default_data       JSON        NULL,
  required_vars      JSON        NULL,
  url_template       TEXT        NULL,
  image_url_template TEXT        NULL,
  enabled            TINYINT(1)  NOT NULL DEFAULT 1,
  created_at         DATETIME(3) NOT NULL,
  updated_at         DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_templates_ver (tenant_id, biz_type, channel, locale, version),
  KEY idx_templates_resolve (tenant_id, biz_type, enabled, version DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

模板按版本不可变：编辑产生新版本而非修改旧版本。这使"最高版本胜出"的解析规则不会改变已发出的消息。

### 解析的回退顺序

`template.Query` 的候选按序尝试：

```
channel+locale → channel → locale → 无
```

通道优先于语言。带邮件 HTML 正文的短信对所有读者都是乱码，而发给中文用户的英文 push 只是次优。需要相反顺序的部署可以调整 `Candidates`。

实现上避免使用

```sql
WHERE channel IN (?, '') AND locale IN (?, '') ORDER BY version DESC
```

该查询会 filesort 且无法使用单一索引覆盖两次回退。应使用两次查询（精确匹配一次，空值回退一次），或在 Go 中对小结果集做格点解析。模板数量在数百量级时两次查询足够；达到数万量级时需要增加 `template_active` 指针表。

## 可选能力

### Transactor

仅接收消息一处需要：消息行与其投递行必须同时写入。没有投递行的消息行对重试机制不可见，永远不会被投递。

`fn` 内的每个操作都必须使用内部 `Store`。使用外层 `Store` 会脱离事务，产生可在回滚后存活的写入。

### Reporter

可选接口，暴露 `Ping` 之外的健康细节。健康检查是否比一次往返更复杂由部署决定，不由库决定。

## 数据量与归档

| 表 | 量级 | 归档 |
|---|---|---|
| `messages` | 每条消息一行 | 按 `created_at` 归档 |
| `deliveries` | 每条消息乘通道数 | 同上。站内信行是用户的消息，不能按普通投递记录删除 |
| `delivery_attempts` | 每次尝试一行，增长最快 | 按季度分区，超期 drop 分区或导出 |
| `delivery_receipts` | 每个事件一行，邮件通道量最大 | 聚合为日粒度漏斗表后归档原始行 |

`deliveries` 上的站内信行有额外保留约束：它不是投递记录，而是用户收件箱中的消息。归档策略必须按通道区分。
