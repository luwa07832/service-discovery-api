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

### 心跳续期

- `POST /api/v1/services/{serviceName}/instances/{instanceId}/heartbeat`
- `POST /api/v1/heartbeat`（批量）

续期只替换目标实例的 `heartbeat_at`，地址、端口、健康状态与权重保持原值，
上报方不必重复提交这些字段。单实例续期请求体只含 `heartbeat_at`
（与注册入口相同，接受 RFC3339 时间或 Unix 秒）：

```json
{"heartbeat_at": "2026-10-01T12:30:00Z"}
```

成功返回 HTTP 200 与完整实例记录：

```json
{"instance":{"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:30:00Z"}}
```

批量续期请求体含非空 `service_name` 与非空 `instances`，每项含 `instance_id` 与 `heartbeat_at`：

```json
{
  "service_name": "billing",
  "instances": [
    {"instance_id": "i-1", "heartbeat_at": "2026-10-01T12:30:00Z"},
    {"instance_id": "i-2", "heartbeat_at": 1759312800}
  ]
}
```

成功返回 HTTP 200，`updated` 为更新条数，`instances` 按请求顺序返回完整实例记录：

```json
{"updated":2,"instances":[{"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:30:00Z"}]}
```

批量写入前校验全部条目：`service_name`、`instance_id` 为空，`instance_id` 重复，
`instances` 缺失或为空，`heartbeat_at` 缺失或无法解析，均返回 HTTP 400
`invalid_parameter` 且不更新任何记录；目标实例不存在时两个入口都返回 HTTP 404
`instance_not_found`，批量同样不部分更新；存储失败返回 HTTP 503
`storage_unavailable`。批量续期只改命中实例的 `heartbeat_at`，不影响其他字段、
未命中记录与后续发现清理行为。

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
- 心跳时间严格超过失联边界：仅当 `evaluate_at` 晚于 `heartbeat_at + heartbeat_timeout` 时判定失联，等于边界仍视为在线。

失联实例从本次查询的候选集合中剔除，且失联清理流程会删除严格超过时限的记录；仍在线的实例以及显式不健康但未超时的记录不被删除、不被修改。
服务名下没有实例，或剔除后没有候选实例，统一返回空列表 `[]`。
返回项包含 `instance_id`、`address`、`port`、`healthy`、`weight`、`heartbeat_at`，
排序规则固定为：权重由高到低；权重相同则按实例标识升序，
保证同一输入输出顺序确定。

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
