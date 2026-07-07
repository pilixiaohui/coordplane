# CoordPlane Session Lifecycle 需求目录

本目录定义 CoordPlane 的 Agent 会话生命周期协议。它描述会话启动前的准备、启动顺序、prepare lease、session pin、会话中反馈、后处理、异常恢复和清理边界。活跃会话内的新消息注入由 message delivery 的 same-turn steering 规则约束。

核心文档：

- `coordplane_agent_session_lifecycle_requirements_2026-07-03.md`

该文档是独立需求说明，可以作为 Go 后端 attempt/session 模块、runner lifecycle、Docker/external runtime 适配和相关测试的开发依据。
