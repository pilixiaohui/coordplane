# CoordPlane 部署到使用全流程

CoordPlane 是一个 local-first 的多 Agent 协作后台:一个 Daemon(coordplane 二进制)管理多个 CLI Agent(Docker 容器内运行),人类通过 `coordplane` 命令或 Web 页面(`web_addr`)与 Agent 对话、派发任务、验收结果。本文档覆盖从零部署到日常使用的完整流程。

## 1. 前置要求

- Linux(macOS 理论上可行,未验证);**Go 1.22+**、**Docker**(daemon 可用)、**git**。
- 磁盘/内存:每个并发 Agent 一个容器;`max_parallel_runs` 控制并发数。
- (可选)真实 Claude live gate:需含 Claude Code 2.1.126 的镜像 + `ANTHROPIC_AUTH_TOKEN`(见 §8)。

## 2. 构建

```sh
git clone https://github.com/pilixiaohui/coordplane.git
cd coordplane
make build                 # 产物在 build/bin/{coordplane,coordlink,coordplane-git-helper}
build/bin/coordplane version
```

## 3. 配置

```sh
sudo mkdir -p /var/lib/coordplane
sudo chmod 0700 /var/lib/coordplane          # data_dir 不得对 group/other 可写
sudo cp docs/coordplane.example.yaml /etc/coordplane.yaml
# 按需编辑 /etc/coordplane.yaml(web_addr 默认 127.0.0.1:8080;关闭前端则留空)
```

所有配置字段见 `coordplane.example.yaml` 注释。关键项:`data_dir`(一切状态)、`operator_socket`(CLI 入口)、`web_addr`(前端入口)、`runtime.default_image`(Agent 镜像)、`max_parallel_runs`(并发上限)。

## 4. 启动

前台运行(调试):

```sh
sudo build/bin/coordplane serve --config /etc/coordplane.yaml
```

systemd(生产式):

```ini
# /etc/systemd/system/coordplane.service
[Unit]
Description=CoordPlane multi-agent daemon
After=docker.service network-online.target
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/coordplane serve --config /etc/coordplane.yaml
Restart=on-failure
User=root

[Install]
WantedBy=multi-user.target
```

```sh
sudo install -m755 build/bin/coordplane /usr/local/bin/coordplane
sudo systemctl daemon-reload && sudo systemctl enable --now coordplane
systemctl status coordplane
```

## 5. 首次使用:创建人类凭据

Daemon 启动后处于 bootstrap 态(无凭据,本地 socket 信任)。**创建凭据后,所有 operator 操作(CLI 与 Web)都要求携带该凭据。**

CLI 方式:

```sh
export COORDPLANE_OPERATOR_SOCKET=/var/lib/coordplane/operator.sock
coordplane credential add --participant participant-owner --secret '请用≥16字符的随机串'
# 之后每个命令都需要:
export COORDPLANE_CREDENTIAL='请用≥16字符的随机串'
coordplane status
```

Web 方式:浏览器打开 `http://127.0.0.1:8080` → 点"首次配置(创建凭据)"输入同一 secret → 之后用"登录"进入。

> 凭据只存 SHA-256 hash;吊销后所有操作立即被拒(轮换/吊销见 §7)。

## 6. 日常使用

### 6.1 添加 Agent

```sh
coordplane agent add --display-name "Agent A" --adapter claude \
  --image node:22-bookworm --instructions-file /etc/coordplane/instructions.md
# 暂停/恢复/归档
coordplane agent pause <agent-id>; coordplane agent resume <agent-id>; coordplane agent archive <agent-id>
```

`instructions-file` 是 Agent 的系统提示文件(建议写明:只用原生 Git 在 /workspace/project 内工作、所有协调动作走 coordlink、不以文字宣称完成)。

### 6.2 添加项目

```sh
# 需要一个 git 仓库作为 C0 起点
coordplane project add --name demo --repo /path/to/repo \
  --ref refs/heads/main --integration-agent <agent-id>
coordplane project list
```

### 6.3 派发任务并监控

```sh
coordplane task create --project <project-id> --agent <agent-id> \
  --title "实现 feature X" --description "任务说明"
coordplane task list --project <project-id>
coordplane status                # 项目/Agent/任务/Run 总览
coordplane task show <task-id>   # 详情(证据类型 captured/human_confirm)
```

Agent 完成后提交结果(`submitted`),人类验收:

```sh
coordplane task accept <task-id> --integration-agent <agent-id>   # 直接 CAS
coordplane task checkout <task-id> --dest ./review                 # 导出精确 head 审查
```

### 6.4 Web 页面

`http://127.0.0.1:8080`:

- Dashboard:项目/Agent/活跃任务总览 + 各 Agent 进度条;
- Agents:每个 Agent 的当前任务、进度、时间线,以及 pause/resume/archive;
- Tasks:看板按状态列,点击任务可 complete(人类任务)/accept/rework/cancel/retry/wake;
- Runs/Messages/Roles/Credentials/Events:运行、收件箱、角色权限、凭据、事件流。

### 6.5 人类参与协作

人类任务(assignee 为 human participant)由 Agent 反向派发:`coordlink task create --participant participant-owner …`(Agent 侧);人类在 Web 或 CLI 完成:

```sh
coordplane task complete <task-id> --summary "审查通过"
```

完成后证据为 `human_confirm`(head_sha 为空),与 Agent 的 `captured` 明确区分。

## 7. 凭据轮换 / 吊销

```sh
coordplane credential rotate --secret '新secret(≥16字符)'
coordplane credential revoke participant-owner   # 吊销后一切操作被拒
# 恢复:用新凭据重新 add(吊销态下仅剩 CLI 本地恢复路径,见故障排查)
```

## 8. (可选)真实 Claude live gate

```sh
digest=$(./tests/e2e/testdata/real-claude/build.sh)   # 构建含 Claude Code 2.1.126 的不可变镜像
E2E_RUNTIME_IMAGE="$digest" E2E_DOCKER_NETWORK=bridge ANTHROPIC_AUTH_TOKEN=… ./scripts/e2e-real-cli.sh
```

SKIP 语义:无镜像/凭据时 gate 跳过,只声明"自动测试通过,live 未验收"。

## 9. 常见故障排查

| 现象 | 处理 |
| --- | --- |
| `SCOPE_DENIED: operator credential is missing, revoked, or invalid` | 设置 `COORDPLANE_CREDENTIAL`(或 Web 登录);已吊销则先 rotate |
| Daemon 启动即退出 | 查日志(`journalctl -u coordplane` / 前台输出);常见:配置缺字段、`data_dir` 不可写、端口被占 |
| Agent Run 失败 `NO_TASK_OUTCOME` | 看 Run 日志(`coordplane run show`);常见:镜像缺 claude/git/go、instructions 指引不清 |
| capture 失败 `workspace must be clean` | Agent 在工作区留下未提交产物;instructions 中要求提交前 `git status --porcelain` 为空 |
| Web 打不开 | 确认 `web_addr` 已配置且未绑定非回环地址;`curl http://127.0.0.1:8080/` 应返回页面 |
| 凭据吊销后无法操作 | 吊销后认证闸关闭一切入口;恢复需人工在数据库外介入——**生产使用请先 rotate 而非 revoke,或保留一个备份凭据流程** |

## 10. 完整自检

```sh
coordplane status                    # daemon_ready=true
coordplane project list              # 至少一个 active 项目
coordplane agent list                # 至少一个 active Agent
coordplane task list                 # 任务可见
curl -s http://127.0.0.1:8080/v1/status -H "X-Coordplane-Credential: $COORDPLANE_CREDENTIAL"  # Web API 可用
```
