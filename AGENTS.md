# AGENTS.md

## 仓库性质

这是公开 agent 客户端仓库。任何提交都必须假设会被所有人读取。

## 禁止内容

- 不提交面板管理员账号或密码。
- 不提交 session secret。
- 不提交设备组安装 token。
- 不提交 node token。
- 不提交 Telegram token。
- 不提交 Cloudflare、R2、Wrangler 账号配置。
- 不提交真实服务器 IP、私钥或生产配置文件。

## 允许内容

- Go agent 源码。
- 单元测试。
- 与公开安装流程相关的无密钥文档。
- 示例配置只能使用占位值。

## 验证命令

```sh
go test ./...
go build ./...
```
