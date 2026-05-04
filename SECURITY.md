# Security Policy

This repository is public and must contain only generic agent client code, tests, and non-secret documentation.

Do not commit:

- panel administrator credentials
- session secrets
- group install tokens
- node tokens
- Telegram bot tokens or chat ids
- Cloudflare, Wrangler, or R2 account configuration
- private keys, SSH configs, real server inventories, or production agent config files

Runtime secrets belong only in the panel datastore or on the target VPS under `/etc/tforward-agent/agent.json`.

Before publishing changes, run:

```sh
go test ./...
rg -n "ADMIN_PASSWORD|SESSION_SECRET|TELEGRAM|CLOUDFLARE|install_token|group_token|node_token|private_key|BEGIN .*PRIVATE KEY|password|secret" .
```

Matches for field names such as `node_token` are expected in source code; real secret values are not.
