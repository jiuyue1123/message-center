# 接入新通道

新增一个通道的成本是实现 `channel.Sender` 并注册。路由、扇出、重试、投递记录、收件箱投影、退订和 PII 脱敏由 dispatcher 对该接口完成，它不感知具体通道。

该性质使 `Sender` 成为接口设计中影响面最大的类型：在此增加一个方法，等于要求所有既有通道实现它；缺少一个方法，等于 dispatcher 必须针对通道做硬编码。方法集合限定为五个。

## 五个方法

| 方法 | 必要性 |
|---|---|
| `Channel()` | 注册表的键、路由的键、按通道 worker 池的划分依据 |
| `Accepts()` | 缺少它，dispatcher 无法从多地址的 `Recipient` 中选择地址，只能硬编码通道到地址类型的映射 |
| `Capabilities()` | 使 dispatcher 能提前拒绝、降级或截断内容，并判断服务商是否支持幂等键 |
| `Validate(ctx, req)` | 使永久性错误在不消耗重试次数、不创建消息的前提下被拒绝 |
| `Send(ctx, req)` | 执行发送 |

## 实现约束

1. **`Validate` 必须离线且无副作用。** 它在请求接收路径上同步执行，覆盖每条消息的每个通道。在其中拨号 SMTP 会使接收路径的 p99 显著上升，且该回归通常会被归因于数据库而非校验器。此约束编译器不强制，依赖 review。

2. **`Send` 必须对失败分类。** 返回带正确 `Kind` 的 `*mcerr.Error`，或 `mcerr.From` 能分类的错误。返回裸 error 会被归类为 `KindUnknown` 并按可重试处理，对瞬时故障正确，对永久故障则浪费重试预算。

3. **`Send` 必须响应 context 取消。** context 触发时返回 `mcerr.FromContext(ctx)` 的结果。被中止的尝试不消耗重试预算；吞掉取消并返回通用错误则会消耗。

4. **实现必须并发安全。** dispatcher 通过 worker 池从多个 goroutine 调用 `Send`。`sync.Once` 保护的懒初始化可行，receiver 上的可变调用状态不可行。

5. **实现不得写入 `Store`。** 持久化由 dispatcher 负责，自行写入 attempt 行会与 dispatcher 竞争。站内信看似例外，实际不是：其 `Send` 为空操作，因为投递行已经存在。

## 站内信实现示例

站内信的传输层即 `deliveries` 表。

```go
package inapp

import (
    "context"
    "time"

    "github.com/jiuyue1123/message-center/pkg/channel"
    "github.com/jiuyue1123/message-center/pkg/mcerr"
    "github.com/jiuyue1123/message-center/pkg/message"
)

type Sender struct {
    Now func() time.Time
}

var _ channel.Sender = (*Sender)(nil)

func (s *Sender) Channel() message.Channel { return message.ChannelInApp }

// 站内信按身份寻址，不接受外部地址。
func (s *Sender) Accepts() []message.AddressForm {
    return []message.AddressForm{message.FormUserID}
}

func (s *Sender) Capabilities() channel.Capabilities {
    return channel.Capabilities{
        SupportsTitle: true,
        SupportsHTML:  true,
        SupportsURL:   true,
        SupportsImage: true,
        SupportsData:  true,
        // 不声明 SupportsReceipts：站内信已读由客户端直接上报，
        // 经 MarkRead 路径，不走回执路径。
        Idempotent: true,
    }
}

func (s *Sender) Validate(_ context.Context, req channel.SendRequest) error {
    if req.Content.IsEmpty() {
        return mcerr.ErrInvalidArgument.WithMessage("in-app message has no content")
    }
    if req.Recipient.UserID == "" {
        return mcerr.ErrInvalidArgument.WithMessage("in-app message has no user_id")
    }
    return nil
}

// Send 不执行操作。投递行已经是收件箱行。
//
// 站内信没有外部传输对象；写入第二张表会使未读数有两个来源，
// 两者会在某次写入失败于其间时开始不一致。
func (s *Sender) Send(_ context.Context, req channel.SendRequest) (message.SendResult, error) {
    now := time.Now()
    if s.Now != nil {
        now = s.Now()
    }
    return message.SendResult{AcceptedAt: now}, nil
}
```

注册：

```go
reg := channel.NewRegistry()
reg.MustRegister(&inapp.Sender{})
```

`Accepts` 返回 `user_id` 是通道声明"自行解析地址"的方式。`RegistryResolver.AllowEmptyAddress` 必须开启，否则站内信会在每条消息上被跳过，因为 `user_id` 不是服务商可拨号的地址。

## SMS 实现要点

```go
func (s *SMSSender) Capabilities() channel.Capabilities {
    return channel.Capabilities{
        SupportsTitle: false,             // dispatcher 会将标题并入正文
        SupportsHTML:  false,
        HTMLPolicy:    channel.HTMLStrip, // 显式声明剥离，不静默发送原始标签
        SupportsData:  false,
        SupportsURL:   true,
        SupportsImage: false,
        Idempotent:    true,              // 多数网关按 OutId 去重
        MaxBodyBytes:  140,               // 超出时截断而非拒绝
    }
}

func (s *SMSSender) Accepts() []message.AddressForm {
    // 顺序表达偏好：优先手机号，其次自行解析 user_id。
    return []message.AddressForm{message.FormPhone, message.FormUserID}
}

func (s *SMSSender) Send(ctx context.Context, req channel.SendRequest) (message.SendResult, error) {
    phone, _ := req.Recipient.FirstOf(message.FormPhone)

    resp, err := s.client.Send(ctx, gateway.Request{
        Phone: phone.Value,
        Body:  req.Content.Body,
        OutID: req.DedupKey(),
    })
    if err != nil {
        if ctxErr := mcerr.FromContext(ctx); ctxErr != nil {
            return message.SendResult{}, ctxErr
        }
        return message.SendResult{}, classify(err)
    }
    return message.SendResult{
        ProviderMessageID: resp.MessageID,
        AcceptedAt:        time.Now(),
        ResponseDigest:    digest(resp.Raw),
        ResponseSnippet:   redact(resp.Raw),
    }, nil
}
```

分类逻辑：

```go
func classify(err error) error {
    var apiErr *gateway.APIError
    if !errors.As(err, &apiErr) {
        // 网络层错误：超时、连接重置。瞬时，可重试。
        return mcerr.Retryable("sms_transport", "gateway unreachable").Wrap(err)
    }
    switch {
    case apiErr.Code == "MOBILE_BLACKLISTED", apiErr.Code == "INVALID_NUMBER":
        // 号码永不可用。短信按次计费，重试有实际成本。
        return mcerr.Permanent("sms_"+apiErr.Code, apiErr.Message).Wrap(err)
    case apiErr.HTTPStatus == 429:
        // 服务商给出了可重试时间，不应自行计算。
        return mcerr.RateLimited(apiErr.RetryAfter, "sms_rate_limited", apiErr.Message).Wrap(err)
    case apiErr.HTTPStatus >= 500:
        return mcerr.Retryable("sms_upstream", apiErr.Message).Wrap(err)
    default:
        return mcerr.From(err)
    }
}
```

`RateLimited` 与 `Retryable` 分开，是因为退避时间由服务商决定。按自身节奏退避会再次被限流并浪费重试次数。

## 注册时的校验

`Registry.Register` 在启动阶段拒绝以下配置：

| 情况 | 原因 |
|---|---|
| `nil` Sender | |
| 空 `Channel()` | 无法路由 |
| 空 `Accepts()` | 该通道永远无法被路由到 |
| `Accepts()` 含未知地址形式 | 拼写错误的形式会静默不匹配 |
| 同一通道注册两次 | 视为错误而非覆盖。覆盖会使配置错误的部署按最后一次注册发送消息 |
| 负的尺寸上限 | |

## 检查清单

```
[ ] Send 对每种失败给出了正确的 Kind
[ ] Validate 中没有任何网络 I/O
[ ] context 取消时返回 mcerr.FromContext(ctx)
[ ] Sender 上没有可变的调用状态
[ ] Send 中不写入 Store
[ ] 服务商支持幂等时，传递了 req.DedupKey()
[ ] 响应中不含未脱敏的 PII（响应体常回显完整请求）
[ ] Capabilities 如实声明
[ ] 有覆盖可重试失败与永久失败两条路径的测试
```

最后一项优先级最高：`classify` 的分支错误是该类集成中最常见的缺陷，且只在服务商出现问题时才暴露。
