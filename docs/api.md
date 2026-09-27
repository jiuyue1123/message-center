# HTTP 接口

Gin 只作为适配层。核心逻辑全部位于 `dispatch.Service`，HTTP 层负责参数绑定、调用 service，以及将 `mcerr.Kind` 映射为状态码。替换 Gin 不影响内核。

本文描述契约，而非某个 handler 的实现。

## 路由

```
POST   /api/v1/messages                          提交消息
GET    /api/v1/messages/:id                      查询消息状态
POST   /api/v1/messages/:id/cancel               取消消息
GET    /api/v1/messages/:id/deliveries           查询投递明细
GET    /api/v1/messages/:id/attempts             查询尝试历史
GET    /api/v1/deliveries/:id/receipts           查询回执时间线

GET    /api/v1/users/:userID/inbox               站内信收件箱
GET    /api/v1/users/:userID/inbox/unread-count  未读数
POST   /api/v1/users/:userID/inbox/read          标记已读

GET    /api/v1/users/:userID/preferences         查询偏好
PUT    /api/v1/users/:userID/preferences         写入偏好

GET    /api/v1/templates                         模板列表
POST   /api/v1/templates                         新建模板版本
GET    /api/v1/templates/:id                     查询模板
POST   /api/v1/templates/:id/disable             停用模板

POST   /api/v1/receipts/:channel                 服务商回执回调

GET    /api/v1/channels                          通道能力清单
GET    /healthz  /readyz                         存活 / 就绪
```

## 设计要点

### 回执回调

`POST /api/v1/receipts/:channel` 由服务商调用，安全模型需要单独说明：

- 鉴权使用 URL 中的签名 token，不使用通用 API key。按服务商分配密钥，使泄露的影响范围可控。
- 必须幂等。重复回执返回 200 而非 409：返回非 2xx 会使服务商持续重试。
- 必须快速返回。仅执行签名校验、落库、返回。响应慢的回调端点会被服务商降速或熔断。

### 收件箱与消息查询是两类接口

`GET /messages/:id` 面向调用方，回答"我提交的这条进展如何"。`GET /users/:userID/inbox` 面向收件人，回答"我的消息"。二者读取 `deliveries` 的不同行，分页、排序、权限模型和返回字段均不同，合并会导致两边都受限。

## POST /api/v1/messages

### 请求

```json
{
  "tenant_id": "acme",
  "idempotency_key": "order-88132-shipped",
  "biz_type": "order_shipped",
  "channels": ["in_app", "push", "email"],
  "recipient": {
    "user_id": "u_10231",
    "display_name": "Ada",
    "locale": "zh-CN",
    "timezone": "Asia/Shanghai",
    "addresses": [
      {"form": "email", "value": "ada@example.com", "verified": true},
      {"form": "device_token", "value": "fcm_abc...", "label": "iPhone 15"}
    ],
    "attributes": {"order_no": "88132", "eta": "周四"}
  },
  "template": {
    "biz_type": "order_shipped",
    "locale": "zh-CN"
  },
  "template_vars": {"order_no": "88132"},
  "channel_content": {
    "sms": {"body": "您的订单 {{.order_no}} 已发出"}
  },
  "priority": "normal",
  "expires_at": "2026-09-29T00:00:00Z",
  "max_attempts": 3,
  "metadata": {"request_id": "req_7f3a"}
}
```

| 字段 | 说明 |
|---|---|
| `idempotency_key` | 建议携带。不带该键的消息无法去重，客户端超时重试会产生第二条消息 |
| `recipient.user_id` | 必填，即使所有投递都发往外部地址。它是站内信收件箱的键、退订偏好的键，也是数据删除请求唯一可用的抓手 |
| `recipient.timezone` | IANA 名称。免打扰按它求值 |
| `template` 与 `content` | 至少一个。同时存在时模板优先 |
| `expires_at` | 不填则使用策略默认值。显式设置才是有效保护：关于限时事件的消息晚到三天，劣于不送达 |

### 响应 202

```json
{
  "message_id": "01JCK8...",
  "status": "pending",
  "deduplicated": false,
  "deliveries": [
    {"id": "01JCK8...A", "channel": "in_app", "status": "pending"},
    {"id": "01JCK8...B", "channel": "push",   "status": "pending"},
    {"id": "01JCK8...C", "channel": "email",  "status": "pending",
     "skip_reason": "biz_type_opt_out"}
  ]
}
```

返回 202 而非 200：提交成功不代表已发送，响应中没有任何内容表明用户收到了什么。

`deduplicated: true` 是成功，不是冲突。客户端在超时后重试时，需要知道的是首次请求已经落地；返回 409 会使其继续重试。

`skip_reason` 直接返回给调用方，不只写入日志。调用方的客服团队会首先被问到"用户为什么没收到"，若必须向消息中心查询才能得到答案，调用方会去建立一份本不该存在的偏好副本。

### 错误 400

```json
{
  "error": {
    "kind": "permanent",
    "code": "invalid_argument",
    "message": "template order_shipped requires variables that were not supplied: order_no",
    "details": [{"channel": "email", "code": "template_missing_var"}]
  }
}
```

## GET /api/v1/messages/:id

```json
{
  "id": "01JCK8...",
  "biz_type": "order_shipped",
  "status": "partially_succeeded",
  "channels": ["in_app", "push", "email"],
  "created_at": "2026-09-27T10:00:00Z",
  "completed_at": "2026-09-27T10:00:04Z",
  "deliveries": [
    {
      "id": "01JCK8...A",
      "channel": "in_app",
      "status": "accepted",
      "milestone": "read",
      "read_at": "2026-09-27T10:03:11Z",
      "attempt_count": 1
    },
    {
      "id": "01JCK8...B",
      "channel": "push",
      "status": "failed",
      "milestone": "none",
      "attempt_count": 3,
      "failure": {
        "kind": "permanent",
        "code": "fcm_unregistered",
        "message": "device token is no longer registered",
        "at": "2026-09-27T10:00:04Z"
      }
    },
    {
      "id": "01JCK8...C",
      "channel": "email",
      "status": "accepted",
      "milestone": "delivered",
      "address": {"form": "email", "value": "a****@example.com"}
    }
  ]
}
```

三点需要注意：

1. `address` 已脱敏。原始地址不出现在任何 HTTP 响应中。
2. `status` 与 `milestone` 是两个维度。`accepted` 表示服务商已接收，`delivered` 表示已到达收件人。
3. 消息处于非终态时应继续轮询。当前没有完成推送通知机制，见 [traps.md](traps.md)。

客户端会在提交后密集轮询该接口，且总会先看到 `pending`。为 `Message` 增加 `callback_url` 并在终态时回调是成本较低的改进；推迟该改动意味着后续每个已有客户端都在轮询。

## GET /api/v1/users/:userID/inbox

```
GET /api/v1/users/u_10231/inbox?limit=20&cursor=eyJ0...&unread=true&biz_type=order_shipped
```

```json
{
  "items": [
    {
      "id": "01JCK8...A",
      "message_id": "01JCK8...",
      "biz_type": "order_shipped",
      "subject": "订单已发出",
      "body": "您的订单 88132 已发出，预计周四送达。",
      "url": "app://orders/88132",
      "read_at": "2026-09-27T10:03:11Z",
      "created_at": "2026-09-27T10:00:00Z"
    }
  ],
  "next_cursor": "eyJ0IjoiMjAyNi0wOS0yN1QxMDowMDowMFoifQ"
}
```

分页使用 keyset（游标），不使用 offset。收件箱按时间倒序且时间单调前移：第 1 页与第 2 页之间有新消息到达时，offset 分页的读者会静默跳过一行。

游标是不透明的。当前编码 `(created_at, id)`，解析它的客户端会在排序规则变更时失效。

响应中没有 `total`，也没有 `has_more`；`next_cursor` 为空表示已到末页。

## GET /api/v1/deliveries/:id/receipts

```json
{
  "items": [
    {"kind": "clicked",   "at": "2026-09-27T10:05:00Z", "received_at": "2026-09-27T10:05:01Z",
     "url": "https://shop.example.com/orders/88132"},
    {"kind": "delivered", "at": "2026-09-27T10:00:30Z", "received_at": "2026-09-27T10:00:31Z"},
    {"kind": "read",      "at": "2026-09-27T10:03:11Z", "received_at": "2026-09-27T10:03:12Z"}
  ],
  "milestone": "clicked"
}
```

顺序是乱的：`clicked` 排在 `read` 之前。回执按事件时间倒序返回，而事件时间不保证单调。

客户端需要"当前状态"时应读取 `milestone`，不应取列表首项。

## POST /api/v1/receipts/:channel

```
POST /api/v1/receipts/email
X-Signature: t=1758967200,v1=5257a8...

{"event_id":"evt_9f2","type":"bounce","message_id":"01JCK8...C",
 "email":"ada@example.com","bounce_type":"hard","timestamp":"2026-09-27T10:01:00Z",
 "reason":"550 5.1.1 user unknown"}
```

| 情况 | 状态码 | 理由 |
|---|---|---|
| 已记录 | 200 | |
| 重复回执 | 200 | 返回非 2xx 会使服务商持续重试 |
| 未知投递 ID | 200 | 多半来自其他环境的回声，重试无意义 |
| 签名无效 | 401 | |
| 请求体格式错误 | 400 | |
| 内部错误 | 500 | 唯一应当使服务商重试的情况 |

## mcerr.Kind 到状态码

| Kind | 状态码 | 使用场景 |
|---|---|---|
| `permanent` | 400 | 参数错误、模板缺失、地址格式非法 |
| `not_found` | 404 | |
| `conflict` | 409 | 仅用于真实冲突。幂等重放返回 202 |
| `rate_limited` | 429 | 携带 `Retry-After` |
| `retryable` | 503 | 携带 `Retry-After` |
| `unknown` | 500 | 未分类 |
| `aborted` | 503 | 服务正在关闭 |

`aborted` 映射到 503 而非 500，因为它表示"请重试"，而客户端对两者的处理通常不同。

## GET /api/v1/channels

暴露 `Registry` 中的能力声明，主要用于排障。

```json
{
  "channels": [
    {
      "channel": "sms",
      "accepts": ["phone", "user_id"],
      "max_body_bytes": 140,
      "supports_title": false,
      "supports_html": false,
      "html_policy": "strip",
      "idempotent": true,
      "supports_receipts": true,
      "retry_policy": {"max_attempts": 1, "backoff_base": "1s"}
    }
  ]
}
```
