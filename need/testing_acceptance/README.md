# CoordPlane 测试验收需求目录

本目录定义 CoordPlane 第一阶段的测试验收口径。它说明哪些测试是必须的、哪些测试只作为发布健康检查、如何把真实失败收敛成低层回归测试，以及哪些测试场景可以直接转化为 Go 测试。

核心文档：

- `coordplane_test_acceptance_requirements_2026-07-04.md`

该文档是独立需求说明，可以作为 Go 后端测试分层、场景矩阵、CI gate、runtime gate、fake CLI 协议测试和真实 CLI release health check 的开发依据。
