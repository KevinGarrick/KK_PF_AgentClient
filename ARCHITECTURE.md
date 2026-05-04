# Agent 架构

## 职责

agent 运行在入口 VPS 上，负责：

- 主动向面板心跳。
- 拉取完整转发配置。
- 执行本机预检查。
- 生成并应用 `nftables` 配置。
- 上报系统指标、规则流量、连接数和落地健康检查结果。

agent 不做：

- 远程 shell 执行。
- WebSSH。
- 多用户鉴权。
- 自动修改落地池。
- iptables fallback。

## 通信

agent 使用本地 `/etc/tforward-agent/agent.json` 中的 `base_url` 和 `node_token`：

- `POST /api/v1/agent/heartbeat`
- `GET /api/v1/agent/config`

node token 由面板动态安装脚本注册后写入本机配置。公开仓库不保存任何 token。

## nftables

agent 只管理：

```text
inet tforward
```

更新流程：

1. 生成完整 nft 配置。
2. 执行 `nft -c -f` 语法检查。
3. 执行 `nft -f` 应用。
4. 执行 `nft -j list table inet tforward` 做轻量检查。
5. 保存 `current.nft` 和 `last-success.nft`。

## 安全

公开仓库只包含通用 agent 代码。生产敏感信息只存在于入口 VPS 本机配置和面板 D1 中。
