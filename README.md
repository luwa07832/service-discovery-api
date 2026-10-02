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
  "port": 8080,
  "healthy": true,
  "weight": 10,
  "heartbeat_at": "2026-10-01T12:00:00Z"
}
```

- `service_name`、`instance_id`：必填非空。
- `address`：访问地址，可选，默认空字符串。
- `port`：端口，可选的非负整数，缺省为 `0`。
- `healthy`：健康状态，显式设置时只接受布尔 `true` 或 `false`，其他取值返回 `invalid_parameter`，缺省按 `false` 处理。
- `weight`：权重，必填且必须大于 0（JSON 数字或数字字符串），小于等于 0 返回 `invalid_parameter` 且原记录不变。
- `heartbeat_at`：心跳时间，必填，接受 RFC3339 时间或 Unix 秒。

### 批量注册 / 覆盖实例

- `POST /api/v1/services/{serviceName}/instances/batch`（服务名取路径值，即使请求体提供 `service_name` 也以路径为准）
- `POST /api/v1/register/batch`（服务名取请求体的 `service_name`）

一次请求登记或覆盖同一服务下多个实例，请求体为单个 JSON 对象并含非空 `instances`
数组；数组每项沿用单实例注册的字段语义：`instance_id` 必填非空且同批不得重复，
`address` 缺省空字符串，`port` 缺省 `0` 且仅接受非负整数，`healthy` 缺省 `false`
且仅接受布尔 `true`/`false`，`weight` 必填且大于 0（JSON 数字或数字字符串），
`heartbeat_at` 必填，接受 RFC3339 时间或 Unix 秒。

```json
{
  "service_name": "billing",
  "instances": [
    {"instance_id": "i-1", "address": "10.0.0.8:8080", "port": 8080, "healthy": true, "weight": 10, "heartbeat_at": "2026-10-01T12:00:00Z"},
    {"instance_id": "i-2", "weight": "5", "heartbeat_at": 1759320300}
  ]
}
```

有效请求按数组顺序登记或覆盖相同 `(service_name, instance_id)` 的记录，成功返回
HTTP 200：`service_name` 为所属服务名，`registered` 为成功处理条目数，
`instances` 为写入的完整记录并严格按请求顺序返回：

```json
{
  "service_name": "billing",
  "registered": 2,
  "instances": [
    {"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:00:00Z"},
    {"service_name":"billing","instance_id":"i-2","address":"","port":0,"healthy":false,"weight":5,"heartbeat_at":"2025-10-01T12:05:00Z"}
  ]
}
```

写入前校验全部条目：请求体不是单个 JSON 对象、`instances` 缺失或为空、
任一条目缺少或重复 `instance_id`，或 `port`、`healthy`、`weight`、
`heartbeat_at` 不合规，均返回 HTTP 400 `invalid_parameter` 单个顶层 `error`
对象，且不创建、不更新任何实例。整批存储失败返回 HTTP 503
`storage_unavailable`，整批回滚、不留下部分更新。批量注册不触发失联清理，
也不改变请求之外的任何实例记录。

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

### 心跳续期

- 单实例：`POST /api/v1/services/{serviceName}/instances/{instanceId}/heartbeat`
- 批量：`POST /api/v1/heartbeat`

续期只需要提交 `heartbeat_at`（与注册入口一致，接受 RFC3339 时间或 Unix 秒），
不必重复提交地址、端口、健康状态和权重；这些字段保持当前值不变。

单实例请求体示例：

```json
{"heartbeat_at": "2026-10-01T12:05:00Z"}
```

成功返回 HTTP 200 与完整实例记录：

```json
{"instance":{"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:05:00Z"}}
```

批量请求体含非空 `service_name` 与非空 `instances`，每项含非空、不重复的
`instance_id` 与可解析的 `heartbeat_at`：

```json
{
  "service_name": "billing",
  "instances": [
    {"instance_id": "i-1", "heartbeat_at": "2026-10-01T12:05:00Z"},
    {"instance_id": "i-2", "heartbeat_at": 1759320300}
  ]
}
```

成功返回 HTTP 200，`updated` 为更新条数，`instances` 中每项都是完整实例记录，
并严格按请求顺序返回：

```json
{
  "updated": 2,
  "instances": [
    {"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:05:00Z"},
    {"service_name":"billing","instance_id":"i-2","address":"10.0.0.9:8080","port":8080,"healthy":false,"weight":5,"heartbeat_at":"2025-10-01T12:05:00Z"}
  ]
}
```

批量写入前校验全部条目：`service_name` 为空、`instances` 缺失或为空、
条目里的 `instance_id` 为空或重复、`heartbeat_at` 缺失或无法解析，
都返回 HTTP 400 `invalid_parameter`，且不更新任何记录。
目标实例不存在时（单实例或批量中的任一条目）返回 HTTP 404
`instance_not_found`，批量也不做部分更新；存储失败返回 HTTP 503
`storage_unavailable`，同样不留下部分更新。
续期不改变未命中记录，也不改变后续发现请求的失联判定与清理行为。

### 按服务名发现健康实例

- `GET|POST /api/v1/services/{serviceName}/discover`
- `GET|POST /api/v1/discover`

参数：`service_name`（必填非空）、`evaluate_at`（评估时刻，必填）、
`heartbeat_timeout`（心跳超时时长，必填且大于零，接受秒数或 `30s`/`2m` 时长文本）。

不可用判定（只针对该服务名下的实例，其他服务记录不变）：

- 健康状态不是健康；
- 心跳时间严格超过失联边界：仅当 `evaluate_at` 晚于 `heartbeat_at + heartbeat_timeout` 时判定失联，等于边界仍视为在线。

失联实例从本次查询的候选集合中剔除，且失联清理流程会删除严格超过时限的记录；仍在线的实例以及显式不健康但未超时的记录不被删除、不被修改。
服务名下没有实例，或剔除后没有候选实例，统一返回空列表 `[]`。
返回项包含 `instance_id`、`address`、`port`、`healthy`、`weight`、`heartbeat_at`，
排序规则固定为：权重由高到低；权重相同则按实例标识升序，
保证同一输入输出顺序确定。

### 服务总览（只读）

- `GET /api/v1/services`

用 `evaluate_at`（必填，接受 RFC3339 时间或 Unix 秒）与
`heartbeat_timeout`（必填且大于零，接受正数秒数或 `30s`、`2m` 时长文本）
生成全部服务的只读快照。失联判定与发现入口一致：仅当 `evaluate_at` 晚于
`heartbeat_at + heartbeat_timeout` 时判定失联，等于边界不算失联。

返回 `services` 数组，只包含仍有实例记录的服务，并按 `service_name`
升序排列；没有任何记录时为空数组。每项字段：

- `service_name`：服务名。
- `total_instances`：当前记录总数。
- `available_instances`：`healthy` 为 `true` 且未失联的实例数。
- `unhealthy_fresh_instances`：`healthy` 为 `false` 但未失联的实例数。
- `lost_instances`：严格失联的实例数。

`total_instances` 恒等于后三项之和，输入相同时计数与顺序确定。示例：

```json
{
  "services": [
    {"service_name":"billing","total_instances":3,"available_instances":1,"unhealthy_fresh_instances":1,"lost_instances":1},
    {"service_name":"gateway","total_instances":1,"available_instances":1,"unhealthy_fresh_instances":0,"lost_instances":0}
  ]
}
```

该入口只读取和汇总 SQLite 记录：不创建、不更新、不删除任何实例，
失联实例只计入 `lost_instances` 而保留在存储中，也不触发发现请求的
失联清理。`evaluate_at` 缺失或无效、`heartbeat_timeout` 缺失或不大于零，
以及 `evaluate_at` 早于任一实例的 `heartbeat_at` 时，返回 HTTP 400
`invalid_parameter` 且不改变记录；存储不可用时返回 HTTP 503
`storage_unavailable`。

### 失联实例清理

- `POST /api/v1/cleanup`

独立的失联清理入口，参数从查询参数或 JSON 对象读取：

- `evaluate_at`：评估时刻，必填，接受 RFC3339 时间或 Unix 秒。
- `heartbeat_timeout`：心跳超时时长，必填且大于零，接受正数秒数或
  `30s`、`2m` 时长文本。
- `service_name`：可选。省略时清理全部服务；显式提供时必须非空，
  且只清理该服务，其他服务的记录不受影响。

失联判定与发现入口一致：仅当 `evaluate_at` 晚于
`heartbeat_at + heartbeat_timeout` 时才删除记录；恰好等于边界、仍在线，
以及显式不健康但未超过时限的实例都保留。请求先完成全部校验再产生删除
结果，本次删除在单个事务中完成，不会留下部分删除。

成功返回 HTTP 200：`evaluate_at` 为本次使用的评估时刻，
`heartbeat_timeout` 为 JSON 秒数，`removed` 为删除数量，`instances`
为刚删除的完整实例记录（`service_name`、`instance_id`、`address`、
`port`、`healthy`、`weight`、`heartbeat_at`），按 `service_name` 升序、
`instance_id` 升序排列；没有记录被删除时 `removed` 为 0 且
`instances` 为空数组。

```json
{
  "evaluate_at": "2026-10-01T12:10:00Z",
  "heartbeat_timeout": 300,
  "removed": 1,
  "instances": [
    {"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:00:00Z"}
  ]
}
```

`evaluate_at` 缺失或无法解析、`heartbeat_timeout` 缺失或不大于零、
显式 `service_name` 为空，或 `evaluate_at` 早于任一待评估实例的
`heartbeat_at`，都返回 HTTP 400 `invalid_parameter` 且不修改任何记录；
存储读写失败返回 HTTP 503 `storage_unavailable`，不留下部分删除结果。

## 参数错误

`service_name` 为空、`instance_id` 为空、`weight` 缺失或不大于 0、
显式 `healthy` 不是布尔 `true`/`false`、
`heartbeat_at` 缺失或无法解析，以及发现请求中 `heartbeat_timeout` 不大于零、
`evaluate_at` 缺失或早于（任一相关）实例心跳时间，统一返回 HTTP 400，
并且不创建、不更新、不删除任何实例记录：

```json
{"error":{"code":"invalid_parameter","message":"service_name must not be empty"}}
```

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

实现只使用上述 SQLite 存储：没有额外持久化文件、后台清理任务或额外数据源；
发现请求触发的失联清理由请求同步完成，仅删除该服务名下严格超过时限的记录。
