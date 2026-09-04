# 前后端分离开发说明

本仓库是 New API 后端受控 Fork，不包含 `web/` 前端源码。前端由 `wildflow-web` 独立开发和部署。
后端采用 **API-only 默认模式**，不再 `go:embed web/dist`。

## 本地启动

准备满足 [go.mod](go.mod) 版本要求的 Go，在本仓 Git 根（包含 `go.mod` 和 `main.go`）打开终端。
使用本地开发配置，不复制生产凭据或数据库。

```bash
go run .
# 默认监听 http://localhost:3000，可用 PORT 覆盖
```

保持该终端运行；前端在另一个终端、自己的 Git 根启动，命令见
[wildflow-web 的 DEV.md](https://github.com/wildsyn/wildflow-web/blob/main/DEV.md)。两仓不必放在相邻目录。

- `FRONTEND_BASE_URL=http://localhost:8080`：把未知浏览器路由 301 到独立前端；
- 不设置：API-only，未知非 `/api` 路由返回 JSON 404。

## 前端调用

- 开发：`wildflow-web` 的 rsbuild 代理 `/api`、`/mj`、`/pg` 到本服务；
- 生产：Nginx 同源反向代理 `/api/*` 到本服务，`/` 到 wildflow-web 静态产物。

## 验证与证据

验证命令见 [README](README.md#本地验证)。在当前改动的 PR 中记录实际执行的命令与结果，
历史通过记录不能替代当前候选的检查；启动进程也不等于模型调用、计费或业务流程已经验收。

## AGPL 义务

公开仓库必须与线上运行 revision 一致；保留 LICENSE、NOTICE、修改标记和“获取源码”入口。
