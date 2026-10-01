# service-discovery-api

把服务实例的注册信息、健康状态、权重和心跳时间记录成可查询的服务，支持按服务名发现健康实例并剔除失联实例。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `service-discovery-api.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### 注册或更新实例

`PUT /v1/instances`（`POST /v1/instances` 同义）或
`PUT /v1/services/{serviceName}/instances/{instanceId}`

请求体：

```json
{
  "service_name": "billing",
  "instance_id": "i-1",
  "address": "10.0.0.1:9000",
  "healthy": true,
  "weight": 10,
  "heartbeat_at": "2026-10-01T12:00:00Z"
}
```

时间戳使用 RFC 3339。相同服务名与实例标识重复注册时，覆盖当前实例记录。
成功返回 HTTP 200 与当前实例记录。

### 查询入口

- `GET /v1/services/{serviceName}/instances/{instanceId}`：返回当前实例记录。
- `GET /v1/services/{serviceName}/instances/{instanceId}/health`：返回健康状态。
- `GET /v1/services/{serviceName}/instances/{instanceId}/weight`：返回权重。
- `GET /v1/services/{serviceName}/instances/{instanceId}/heartbeat`：返回心跳时间。
- `DELETE /v1/services/{serviceName}/instances/{instanceId}`：删除当前实例记录；
  也可用 `DELETE /v1/instances?service_name=...&instance_id=...`。

### 发现健康实例

`GET /v1/services/{serviceName}/instances?evaluate_at=...&timeout=...`
（扁平入口：`GET /v1/instances?service_name=...&evaluate_at=...&timeout=...`）

参数：

- `evaluate_at`：评估时刻，RFC 3339 时间戳。
- `timeout`：心跳超时时长，Go duration（如 `30s`）或以秒为单位的正数。

发现只处理指定服务名的实例。心跳时间早于或等于 `evaluate_at - timeout` 的实例
视为失联，并在本次查询中从可发现实例集合剔除；健康状态不是健康的实例保留在存储中、
但不在结果中返回。返回项保留实例标识、访问地址、健康状态、权重和心跳时间，排序规则为：

1. 权重由高到低；
2. 权重相同时，心跳时间由新到旧；
3. 仍相同时，实例标识升序。

服务名下没有实例，或剔除后没有候选实例时，返回空列表：

```json
{
  "service_name": "billing",
  "evaluate_at": "2026-10-01T12:00:10Z",
  "timeout": "10s",
  "instances": []
}
```

## 参数规则

以下情况统一返回 HTTP 400 `invalid_parameter`，并且不创建、不更新、不删除任何实例记录：

- 服务名或实例标识为空；
- 权重缺失或不是非负数；
- 注册/更新时心跳时间缺失或格式不合法；
- 发现时 `timeout` 不大于零、`evaluate_at` 缺失或格式不合法；
- `evaluate_at` 早于该服务名下任一实例的心跳时间。

实例记录不存在时，查询与删除返回 HTTP 404 `instance_not_found`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；
`message` 不包含 SQL、堆栈或文件路径。
