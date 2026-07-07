# CoordPlane Backend 需求目录

本目录定义 CoordPlane 后台服务第一阶段实现范围。当前开发目标先完成单用户后台核心闭环，同时建立 capability registry、skill registry、TeamConfig、SecretProvider MVP，并为多用户、远程 runner、分布式队列、完整 secret vault、UI 和长期记忆保留稳定接口。Skill 和 TeamConfig 的详细边界分别见 `../skills/` 与 `../team_config/`。

核心文档：

- `coordplane_single_user_backend_mvp_requirements_2026-07-03.md`

该文档是独立需求说明，可以作为 Go 后端 service、store、queue、policy、capability registry、skill registry、TeamConfig、coordlink backend API、schema-derived tool adapter 和 operator inspect API 的第一阶段开发依据。
