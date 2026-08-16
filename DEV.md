# 前后端分离开发说明

本仓库是 New API 后端受控 Fork，不包含 `web/` 前端源码。前端由 `wildflow-web` 独立开发和部署。
后端已经改为 **API-only 默认模式**，不再 `go:embed web/dist`，`go build ./...` 可通过。

## 本地启动

```bash
go run .
# 默认监听 http://localhost:3000，可用 PORT 覆盖
```

- `FRONTEND_BASE_URL=http://localhost:8080`：把未知浏览器路由 301 到独立前端；
- 不设置：API-only，未知非 `/api` 路由返回 JSON 404。

## 前端调用

- 开发：`wildflow-web` 的 rsbuild 代理 `/api`、`/mj`、`/pg` 到本服务；
- 生产：Nginx 同源反向代理 `/api/*` 到本服务，`/` 到 wildflow-web 静态产物。

## 已验证

- `go build ./...` 通过；
- `go test ./router/...` 通过。

## AGPL 义务

公开仓库必须与线上运行 revision 一致；保留 LICENSE、NOTICE、修改标记和“获取源码”入口。
