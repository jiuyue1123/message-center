# message-center

Go 实现的消息中心，统一收敛站内信、移动 Push、邮件、短信、Webhook 五类出站通知的发送通道。

用于解决多套通知链路各自实现所带来的问题：重复的 SDK 接入、不一致的重试策略、分散的投递记录，以及无法统一实施的用户偏好与频控。

两种使用方式：

- **作为服务**：部署后通过 HTTP 接口提交消息
- **作为库**：以 `dispatch.Service` 嵌入现有进程，直接调用

## 状态

当前处于接口定义阶段。核心接口、领域模型、状态机、错误分类、数据库表结构与 HTTP 契约已完成，全部可编译并通过 `go doc` 查阅。

通道实现、worker 池、HTTP handler 与数据库实现尚未提供。**当前不可用于生产环境。**

## 特性

**发送**

- 一次提交扇出到多个通道，每个通道独立重试、独立记录
- 异步投递：提交立即返回 `messageID`，worker 池后台消费
- 指数退避重试，带 jitter 以避免重试风暴
- 幂等键去重，避免客户端超时重试产生重复消息
- 投递语义为至少一次，配合服务商幂等键降低重复

**可观测**

- 逐通道投递状态与完整的尝试历史
- 送达追踪：回执（送达、已读、点击、退信、投诉）与单调里程碑
- 支持服务商回执回调，容忍乱序与重复投递

**用户偏好**

- 按通道、按事件类型的退订
- 地址抑制，用于硬退信
- 免打扰时段，按收件人时区求值，支持跨午夜与业务豁免
- 频控，使用滑动窗口

**内容**

- 模板按业务类型、通道、语言、版本解析，支持逐级回退
- 在接收时渲染并快照，重试不重新渲染
- 站内信收件箱：列表、未读数、标记已读

**扩展**

- 通道通过实现 `channel.Sender` 五方法接口接入
- 通道能力声明，dispatcher 据此提前拒绝或降级内容
- 错误分类驱动重试决策，区分可重试与永久失败
- 核心不依赖任何第三方库

## 快速开始

环境要求：Go 1.27 或更高版本。

```bash
git clone git@github.com:jiuyue1123/message-center.git
cd message-center

make check        # 格式校验、vet、编译、竞态检测测试
make help         # 列出全部可用目标
```

查阅接口：

```bash
go doc ./pkg/channel    Sender    # 通道扩展点
go doc ./pkg/store      Store     # 持久化契约
go doc ./pkg/dispatch   Service   # 对外服务接口
go doc ./pkg/message    Delivery  # 核心领域类型
```

接入自定义通道需要实现 `channel.Sender` 并注册：

```go
type Sender interface {
    Channel() message.Channel
    Accepts() []message.AddressForm
    Capabilities() Capabilities
    Validate(ctx context.Context, req SendRequest) error
    Send(ctx context.Context, req SendRequest) (message.SendResult, error)
}
```

```go
reg := channel.NewRegistry()
reg.MustRegister(&mySender{})
```

完整实现步骤、约束与检查清单见 [docs/adding-a-channel.md](docs/adding-a-channel.md)。

## 目录结构

```
pkg/                    公开 API，可被外部项目 import
├── mcerr/              错误分类
├── message/            领域模型与状态机
├── channel/            Sender 扩展点、能力声明、限流
├── template/           模板解析与渲染
├── queue/              工作队列契约
├── preference/         用户偏好：退订、免打扰、频控
├── store/              持久化契约
└── dispatch/           编排：扇出、worker 契约、重试策略

docs/                   设计文档
internal/               待建：Gin 适配层、MySQL 实现、通道 Sender
cmd/                    待建：服务入口
```

`pkg/` 与 `internal/` 的划分：外部使用者需要 import 的放 `pkg/`，绑定具体厂商或框架的放 `internal/`。依赖方向无环，`mcerr` 为叶子包。

## 文档

| 文档 | 内容 |
|---|---|
| [architecture.md](docs/architecture.md) | 分层、领域模型、状态机、时序图、恢复机制 |
| [preferences.md](docs/preferences.md) | 两阶段偏好求值、免打扰、频控、回执与里程碑 |
| [storage.md](docs/storage.md) | Store 契约、MySQL 表结构与索引设计依据 |
| [api.md](docs/api.md) | HTTP 路由、报文格式、错误码映射 |
| [adding-a-channel.md](docs/adding-a-channel.md) | Sender 契约、实现示例、检查清单 |
| [traps.md](docs/traps.md) | 22 项已知问题与待决策事项 |

[traps.md](docs/traps.md) 记录了当前设计尚未解决的问题，其中包含上线前需要确定的事项。

## 后续工作

1. `pkg/store/memstore`：内存实现与一致性测试
2. `internal/channel/inapp`：无厂商依赖，用于验证 Sender 契约
3. 内存队列与 worker 池
4. `dispatch` 实现及三个恢复机制（sweeper、reclaimer、scheduler）
5. `internal/transport/gin`：HTTP 适配层
6. `internal/store/mysql`：按 [storage.md](docs/storage.md) 的表结构实现

第 4 步为必选项。队列是加速器而非真相来源，缺少这三个恢复机制时，队列丢弃的任何任务都会使对应消息永久搁浅。

### 上线前需要确定

- [ ] 幂等键的保留期（[traps.md #1](docs/traps.md)）
- [ ] 广播是否在路线图上。若在，需要增加第四层模型（[traps.md #3](docs/traps.md)）
- [ ] 是否需要完成回调。当前增加成本较低，推迟意味着每个客户端都在轮询（[traps.md #4](docs/traps.md)）
- [ ] 多副本部署下的共享限流与频控计数器（[traps.md #6](docs/traps.md)）
- [ ] 投诉自动降级的阈值动作，接入真实邮件服务商前需要补齐（[traps.md #19](docs/traps.md)）
