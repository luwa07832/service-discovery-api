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

### 注册 / 更新实例

- `POST /api/v1/services/{serviceName}/instances`
- `PUT /api/v1/services/{serviceName}/instances/{instanceId}`
- `POST /api/v1/register`、`POST/PUT /api/v1/instances`

同一 `(service_name, instance_id)` 重复注册会整体覆盖当前实例记录。请求体示例：

```json
{
  "service_name": "billing",
  "instance_id": "i-1",
  "address": "10.0.0.8:8080",
  "healthy": true,
  "weight": 10,
  "heartbeat_at": "2026-10-01T12:00:00Z"
}
```

- `service_name`、`instance_id`：必填非空。
- `address`：访问地址，可选，默认空字符串。
- `healthy`：健康状态，接受布尔值或 `healthy`/`unhealthy` 等文本，缺省按 `false` 处理。
- `weight`：权重，必填的非负数（JSON 数字或数字字符串）。
- `heartbeat_at`：心跳时间，必填，接受 RFC3339 时间或 Unix 秒。

### 公开查询

- 实例完整记录：`GET /api/v1/services/{serviceName}/instances/{instanceId}`
- 服务下全部实例：`GET /api/v1/services/{serviceName}/instances`（可加 `healthy=true/false` 过滤，不会删除任何记录）
- 健康查询：`GET /api/v1/services/{serviceName}/instances/{instanceId}/health`
- 权重查询：`GET /api/v1/services/{serviceName}/instances/{instanceId}/weight`
- 心跳查询：`GET /api/v1/services/{serviceName}/instances/{instanceId}/heartbeat`
- 删除实例：`DELETE /api/v1/services/{serviceName}/instances/{instanceId}`（不存在返回 404）

路径风格与查询参数风格等价，例如
`GET /api/v1/instances?service_name=billing&instance_id=i-1`。
删除另有 `POST /api/v1/services/{serviceName}/instances/{instanceId}/delete`
与 `POST /api/v1/deregister`。

### 按服务名发现健康实例

- `GET|POST /api/v1/services/{serviceName}/discover`
- `GET|POST /api/v1/discover`

参数：`service_name`（必填非空）、`evaluate_at`（评估时刻，必填）、
`heartbeat_timeout`（心跳超时时长，必填且大于零，接受秒数或 `30s`/`2m` 时长文本）。

不可用判定（只针对该服务名下的实例，其他服务记录不变）：

- 健康状态不是健康；
- 心跳时间早于或等于 `evaluate_at - heartbeat_timeout`。

失联实例只从本次查询的候选集合中剔除，数据库中的实例记录不被删除、不被修改。
服务名下没有实例，或剔除后没有候选实例，统一返回空列表 `[]`。
返回项包含 `instance_id`、`address`、`healthy`、`weight`、`heartbeat_at`，
排序规则固定为：权重由高到低；权重相同则心跳时间由新到旧；
再相同则按实例标识升序，保证同一输入输出顺序确定。

## 参数错误

`service_name` 为空、`instance_id` 为空、`weight` 缺失或不是非负数、
`heartbeat_at` 缺失或无法解析，以及发现请求中 `heartbeat_timeout` 不大于零、
`evaluate_at` 缺失或早于（任一相关）实例心跳时间，统一返回 HTTP 400，
并且不创建、不更新、不删除任何实例记录：

```json
{"error":{"code":"invalid_parameter","message":"service_name must not be empty"}}
```

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

实现只使用上述 SQLite 存储：没有额外持久化文件、后台清理任务或额外数据源；
发现过程中的失联剔除仅是查询时过滤。
