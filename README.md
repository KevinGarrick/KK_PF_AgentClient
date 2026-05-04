# KK PF Agent Client

公开的入口节点 agent 源码仓库。

生产安装脚本会从本仓库 clone 指定 ref，并在目标 Linux 节点本地构建 `tforward-agent`。本仓库不得保存任何控制端密钥、安装 token、node token、Telegram token、服务器密码、私钥、Cloudflare 配置或生产环境配置。

## 本地验证

```sh
go test ./...
go build -trimpath -ldflags "-s -w" -o tforward-agent .
```

## 运行方式

agent 运行时只从节点本地配置文件读取控制端地址和 `node_token`：

```sh
./tforward-agent --config /etc/tforward-agent/agent.json
```

配置文件由控制端安装脚本在节点注册后生成，不应提交到本仓库。
