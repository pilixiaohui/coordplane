# wordfreq — 并发开发契约

wordfreq 是一个命令行词频统计程序(Go):从标准输入读取文本,输出每个词的出现次数。

## 模块边界(并发开发,严禁越界)

| 文件 | 负责人 | 契约 |
| --- | --- | --- |
| `tokenize.go` | P1(agentA) | `func Tokenize(text string) []string`:把文本切成词元。词 = 连续字母/数字序列,转小写,忽略其余字符(标点、空白、换行)。示例:`"Hello world hello."` → `["hello","world","hello"]`。只修改本文件。 |
| `count.go` | P2(agentB) | `func Count(words []string) map[string]int`:统计词元出现次数。示例:`["hello","world","hello"]` → `{"hello":2,"world":1}`。只修改本文件。 |
| `main.go` | 已提供,勿改 | 子命令 `tokenize` / `count` / 默认 full 管道,输出排序的 `词 次数` 行。 |

## 验收

`./fixture-test.sh <mode>` 必须通过:`tokenize` 模式验证 Tokenize 契约,`count` 模式验证 Count 契约,`full` 模式验证完整管道。`go build ./...` 必须通过。

## 禁止

- 修改 `main.go`、`SPEC.md`、`fixture-test.sh`。
- 依赖另一个模块的实现细节(两个模块在各自仓库独立验收)。
- 在代码或提交信息中写入任何凭据。
