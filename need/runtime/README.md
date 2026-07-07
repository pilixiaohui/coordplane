# CoordPlane Runtime 需求目录

本目录定义 CoordPlane 的运行环境协议。它描述 Docker 隔离、external runtime、runner、CLI Agent 适配和容器内本地客户端 `coordlink`。Agent 通过 skills 理解工作流，通过 `coordlink` 调用 backend capability。实时消息注入由 message delivery 模块发起，runtime/runner 只提供 CLI adapter 能力。

核心文档：

- `coordplane_local_client_coordlink_2026-07-03.md`

该文档是独立需求说明，可以作为 Go runner、Docker runtime、external runtime 和 `coordlink` 客户端的开发依据。它不依赖任何历史实现。
