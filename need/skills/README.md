# CoordPlane Skills 需求目录

本目录定义 CoordPlane 的 skills-first Agent 工作流层。它说明 skill 如何被注册、启用、读取、版本化，以及 skill 与 backend capability API 的关系。

核心文档：

- `coordplane_skills_first_agent_workflow_requirements_2026-07-03.md`

该文档是独立需求说明，可以作为 Go 后端 skill registry、TeamConfig skill binding、coordlink skill 命令、Agent bootstrap prompt 和相关测试的开发依据。skill 不承担机器 schema；schema-derived tool adapter 必须复用同一 capability registry。
