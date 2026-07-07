# CoordPlane Message Delivery 需求目录

本目录定义 CoordPlane 的实时消息递送协议。它描述 backend 如何把新的 mailbox 事项以轻量 signal 注入到正在运行的 CLI Agent 会话中，并让 Agent 主动通过 CoordPlane 接口读取、回复和继续工作。

核心文档：

- `coordplane_live_message_delivery_requirements_2026-07-03.md`

该文档是独立需求说明，可以作为 Go 后端 delivery service、runner steer adapter、CLI backend adapter、coordlink mailbox / communication capability 和相关测试的开发依据。它不依赖任何历史实现。
