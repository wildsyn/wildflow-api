# wildflow-api

野生流动 1.0 商业控制面后端。本仓库是 [QuantumNous/new-api](https://github.com/QuantumNous/new-api)
的受控 Fork。

## 定位

- 用户、API Key、模型商品、价格、余额、账单、订阅和公开模型/任务 API；
- 公开任务入口与客户侧渠道路由；
- 不承载：自建 GPU 生命周期、Worker Lease、模型部署、渠道凭据和 GPU 运维（这些在私有
  `wildflow-inference` 仓库）。

## 许可证与可见性

- 许可证：AGPL-3.0（继承上游）；
- 可见性：公开仓库；
- 线上部署 revision 必须与公开源码一致，并保留 `LICENSE`、`NOTICE`、修改标记和“获取源码”入口。

## 上游基线

- 上游：https://github.com/QuantumNous/new-api
- 锁定 Release：`v1.0.0-rc.24`
- 锁定上游 commit：`5c3abffe8572aa8a49f15c3916707d2019d66af4`
- 本仓库排除了上游 `web/` 前端目录；前端受控 Fork 位于 `wildflow-web`。
- 上游原 README 保留在 `UPSTREAM-README.md`。

## 边界

- `wildflow-web`：浏览器和公开页面通过 `/api` 同源调用本服务；
- `wildflow-inference`：通过内部 service-to-service 幂等接口/事件调用，不维护余额和退款状态；
- 不复制生产用户、Key、余额、账单或运行配置到本仓库。

## 本地验证

```bash
go build ./...
go test ./...
GOWORK=off go test ./relaykit/...
bash scripts/check-local.sh
```

默认运行模式是 API-only；前端由 `wildflow-web` 独立部署。`FRONTEND_BASE_URL` 只提供显式浏览器
重定向，不恢复单二进制内嵌前端。
