# API-only 默认模式 TDD 证据

日期：2026-08-17。

## RED

- 先添加 `router/api_only_test.go`；
- 命令：`go test ./router -run TestSetRouterDefaultsToAPIOnly -count=1`；
- 结果：失败，未知浏览器路径 `/dashboard` 实际返回 200，而契约要求默认返回 JSON 404；
- checkpoint：`820a89ab`。

## GREEN

- 移除默认 `go:embed web/dist` 依赖；
- 未设置前端兼容环境变量时，未知浏览器路径返回 JSON 404；
- `FRONTEND_BASE_URL` 重定向模式保留；不保留单二进制内嵌前端路径；
- 同一测试转绿，生产改动 checkpoint：`6e4ea1e3`。

## 回归与安全边界

- `go build ./...` 通过；
- `go test ./...` 通过：87 个 package、1150 个测试；
- `cd relaykit && GOWORK=off go test ./...` 通过；
- `bash scripts/check-local.sh` 通过；
- 未改变 `/api`、`/v1`、认证、账本或渠道路由语义；默认 404 不返回内部拓扑或凭据。
