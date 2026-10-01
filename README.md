# service-discovery-api

把服务实例的注册信息、健康状态、权重和最近心跳时间统一记录在进程内，可按服务名发现
健康且未失联的实例，并通过清理入口删除失联实例。

## 运行要求

- Go 1.26 或以上

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`，可用环境变量 `ADDR` 覆盖。

实例记录只保存在进程内存中：没有数据库文件、磁盘写入或后台任务，进程重启后记录清空。

## 已公开的入口

### `GET /healthz`

进程内存储始终可用，正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

### 注册 / 更新实例

- `POST /api/v1/services/{serviceName}/instances`
- `PUT /api/v1/services/{serviceName}/instances/{instanceId}`
- `POST /api/v1/register`、`POST/PUT /api/v1/instances`

同一 `(service_name, instance_id)` 重复注册会整体覆盖当前实例记录，服务名与实例 ID
的关联保持不变。请求体示例：

```json
{
  "service_name": "billing",
  "instance_id": "i-1",
  "host": "10.0.0.8",
  "port": 8080,
  "healthy": true,
  "weight": 10,
  "heartbeat_at": "2026-10-01T12:00:00Z",
  "request_at": "2026-10-01T12:00:00Z"
}
```

- `service_name`、`instance_id`：必填非空。
- `host`：主机地址，必填非空；`port`：必填，`0–65535` 的整数。
  也兼容只提供旧字段 `address`（`host:port` 文本）的请求。
- `healthy`：健康状态，显式提供时只接受布尔值 `true` 或 `false`
  （查询参数中为文本 `true`/`false`），其他取值（数字、`healthy`/`unhealthy`
  等文本）一律返回 `INVALID_ARGUMENT`；缺省按 `false` 处理。
- `weight`：初始权重，必填且严格大于零（JSON 数字或数字字符串）；
  零或负数返回 `INVALID_ARGUMENT`，且保持原记录不变。
- `request_at`：请求发生的时刻，必填，接受 RFC3339 时间或 Unix 秒
  （键名也接受 `now`/`at`/`request_time`）。
- `heartbeat_at`：最近心跳时刻，可选；缺省取 `request_at`。
  心跳时刻不得晚于请求时刻。

成功返回 HTTP 200，`registered` 恒为 true，`already_registered` 反映该实例此前是否
已记录，`instance` 是当前记录（含 `host`、`port`、`address`、`healthy`、`weight`、
`heartbeat_at`）：

```json
{
  "registered": true,
  "already_registered": false,
  "instance": {
    "service_name": "billing",
    "instance_id": "i-1",
    "host": "10.0.0.8",
    "port": 8080,
    "address": "10.0.0.8:8080",
    "healthy": true,
    "weight": 10,
    "heartbeat_at": "2026-10-01T12:00:00Z"
  }
}
```

### 心跳更新

- `POST/PUT /api/v1/services/{serviceName}/instances/{instanceId}/heartbeat`
- `POST/PUT /api/v1/heartbeat`

实例注册后可提交包含 `healthy`、`weight`、`heartbeat_at` 的更新，请求时刻仍由
`request_at` 提供。每次有效更新都会同时刷新最近心跳时间以及最新状态和权重；
未提供的 `healthy`、`weight` 保持现值；`healthy` 显式提供时只接受 `true`/`false`，
`weight` 显式提供时必须严格大于零，否则返回 `INVALID_ARGUMENT` 且记录不变；
`heartbeat_at` 缺省取 `request_at`，且不得晚于请求时刻。

实例 ID 不存在时不创建实例，返回 HTTP 200 与 `"updated": false`：

```json
{"updated": false, "service_name": "billing", "instance_id": "ghost"}
```

更新成功返回 `"updated": true` 与刷新后的 `instance` 记录。同一实例在完全相同的
心跳时刻重复提交相同内容，结果确定，不会产生新实例。

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

参数：`service_name`（必填非空）、`evaluate_at`（发现时刻，必填）、
`heartbeat_timeout`（失联阈值，必填且大于零，接受秒数或 `30s`/`2m` 时长文本）。

只处理该服务名下的实例：

- 健康状态不是健康的实例不出现；
- 最近心跳时间**早于** `evaluate_at - heartbeat_timeout` 的实例视为失联、不出现；
  心跳时刻恰好等于该边界的实例仍出现。

发现只做查询时过滤，不删除、不修改任何记录。服务名下没有记录、没有健康实例或全部
失联时统一返回空列表 `[]`，而不是错误。返回项包含 `instance_id`、`host`、`port`、
`address`、`healthy`、`weight`、`heartbeat_at`，排序固定为：

1. 权重由高到低；
2. 权重相同则按实例 ID 升序。

### 清理失联实例

- `POST /api/v1/services/{serviceName}/cleanup`
- `POST /api/v1/cleanup`

参数与发现相同（`service_name`、`evaluate_at`、`heartbeat_timeout`）。清理会从记录中
删除该服务下最近心跳时间早于 `evaluate_at - heartbeat_timeout` 的实例（不区分健康
状态），并返回被删除的实例 ID 集合；服务名没有记录时返回空集合。发现与清理是两个
独立入口，只有清理会删除记录。

```json
{
  "service_name": "billing",
  "evaluate_at": "2026-10-01T12:10:00Z",
  "heartbeat_timeout": 600,
  "deleted_instance_ids": ["i-2", "i-7"]
}
```

## 参数错误

服务名或实例 ID 为空、主机地址或端口缺失/非法、请求时刻缺失，健康状态取值不是
`true`/`false`、权重小于等于零、心跳时刻晚于请求时刻，以及发现/清理请求中
`heartbeat_timeout` 不大于零或 `evaluate_at` 缺失/非法，统一返回 HTTP 400
`INVALID_ARGUMENT`，并且不创建、不更新、不删除任何实例记录（已存在记录保持不变）：

```json
{"error":{"code":"INVALID_ARGUMENT","message":"service_name must not be empty"}}
```

除上述参数错误、心跳更新未知实例（HTTP 200 `"updated": false`）、查询/删除未知实例
（HTTP 404 `instance_not_found`）和空发现结果（HTTP 200 空列表）外，没有其他失败
结果，也没有任何落盘行为。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段。
