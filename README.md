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

### 路径定位优先

凡是路径中包含服务名或实例标识的入口（单实例登记与覆盖、实例及属性
查询、实例列表、单项心跳续期、单项权重修改、删除、按服务发现），对应
定位字段始终以路径值为准：

- 请求体或查询参数给出的冲突值、空值、非字符串值以及该字段已支持的
  同义字段（如 `serviceName`/`service`、`instanceId`/`instance`/`id`）
  都不能改变目标；
- 仅当路径未提供某个定位段时（例如参数风格入口或
  `POST /api/v1/services/{serviceName}/instances` 只在路径中给出服务名），
  该字段才沿用既有的请求体/查询参数读取规则——因此登记路径只指定服务名时，
  `instance_id` 仍从现有请求参数取得；
- 路径定位段去除首尾空白后为空（包括已识别的空路径段，如
  `/api/v1/services//instances/i-1`）一律返回 HTTP 400 `invalid_parameter`，
  不能由请求体或查询参数补齐。

例如
`GET /api/v1/services/alpha/instances/i-1?service_name=beta&instance_id=i-2`
始终读取 alpha 的 i-1，响应中的 `service_name`、`instance_id` 也与路径一致；
覆盖、续期、权重修改和删除同样只作用于路径目标。路径目标不存在时，单实例
读取、续期、权重修改和删除统一返回 HTTP 404 `instance_not_found`，即使冲突
参数指向另一个存在的实例也不回退；登记入口保留创建或整条覆盖语义，无记录
服务的列表与发现仍返回 HTTP 200 与空实例数组 `[]`。

其他业务字段（`weight`、`port`、`healthy`、`heartbeat_at`、`evaluate_at`、
`heartbeat_timeout`、`address` 等）的来源优先级、校验与默认值保持不变。

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
- `port`：端口，可选的非负整数（`0` 到 `9223372036854775807`，含端点），缺省为 `0`。
- `healthy`：健康状态，显式设置时只接受布尔 `true` 或 `false`，其他取值返回 `invalid_parameter`，缺省按 `false` 处理。
- `weight`：权重，必填且必须大于 0（JSON 数字或数字字符串），小于等于 0 返回 `invalid_parameter` 且原记录不变。
- `heartbeat_at`：心跳时间，必填，接受 RFC3339 时间或 Unix 秒。

### 批量注册 / 覆盖实例

- `POST /api/v1/services/{serviceName}/instances/batch`（服务名取路径值，即使请求体提供 `service_name` 也以路径为准）
- `POST /api/v1/register/batch`（服务名取请求体的 `service_name`）

一次请求登记或覆盖同一服务下多个实例，请求体为单个 JSON 对象并含非空 `instances`
数组；数组每项沿用单实例注册的字段语义：`instance_id` 必填非空且同批不得重复，
`address` 缺省空字符串，`port` 缺省 `0` 且仅接受 `0` 到
`9223372036854775807`（含端点）的整数，`healthy` 缺省 `false`
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

### 只修改实例权重

- 单实例：`PUT /api/v1/services/{serviceName}/instances/{instanceId}/weight`
- 批量：`PUT /api/v1/services/{serviceName}/instances/weight`

调用方只需要提交新权重，不必重复提交地址、端口、健康状态和心跳时间；
这些字段逐字段保持当前值不变，注册入口的整条覆盖语义也不改变。
`weight` 沿用注册语义：仅接受 JSON 数字或数字字符串中的有限正数。

单实例从路径读取服务名与实例标识，`weight` 从 JSON 请求体或查询参数读取；
两者同时给出时以 JSON 请求体为准。请求体示例（等价于
`PUT /api/v1/services/billing/instances/i-1/weight?weight=20`，二者同时出现
时以请求体为准）：

```json
{"weight": 20}
```

成功返回 HTTP 200，顶层 `instance` 为修改后的完整记录：

```json
{"instance":{"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":20,"heartbeat_at":"2026-10-01T12:00:00Z"}}
```

批量请求体是单个 JSON 对象，`updates` 为非空数组；每项含 `instance_id`
与 `weight`，同批 `instance_id` 不得重复：

```json
{
  "updates": [
    {"instance_id": "i-2", "weight": "9.5"},
    {"instance_id": "i-1", "weight": 100}
  ]
}
```

请求先完整校验并确认所有目标存在，全部成立后才在一个事务里统一修改。
成功返回 HTTP 200，`updated` 为修改条数，`instances` 严格按请求顺序包含
修改后的完整记录：

```json
{
  "updated": 2,
  "instances": [
    {"service_name":"billing","instance_id":"i-2","address":"10.0.0.9:8080","port":8080,"healthy":false,"weight":9.5,"heartbeat_at":"2025-10-01T12:05:00Z"},
    {"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":100,"heartbeat_at":"2026-10-01T12:00:00Z"}
  ]
}
```

请求体不是单个 JSON 对象、服务名或实例标识为空、`weight` 缺失、不可解析、
非有限值或不大于 0，以及批量的 `updates` 缺失、为空、不是数组、条目不是
对象、缺少 `instance_id` 或同批重复，均返回 HTTP 400 `invalid_parameter`
且不修改任何记录。目标实例不存在时（单实例或批量中的任一条目）返回
HTTP 404 `instance_not_found`，批量整批不生效；存储失败返回 HTTP 503
`storage_unavailable`，不留下部分更新。修改后权重查询
（`GET .../instances/{instanceId}/weight`）与发现结果继续按权重降序、
实例标识升序排列。

### 只修改实例健康状态

- 单实例：`PUT /api/v1/services/{serviceName}/instances/{instanceId}/health`
- 批量：`PUT /api/v1/services/{serviceName}/instances/health`

调用方只需要提交新的健康状态，不必重复提交地址、端口、权重和心跳时间；
这些字段逐字段保持当前值不变，服务名与实例标识也不变。入口不会创建不
存在的实例，不会续期心跳，也不会触发发现入口中的失联删除；注册入口的
整条覆盖语义同样保持不变。

单实例从路径读取服务名与实例标识，`healthy` 从 JSON 请求体或查询参数
读取，两者同时给出时以 JSON 字段为准。JSON 中 `healthy` 只接受布尔
`true` 或 `false`；查询参数只接受小写文本 `true` 或 `false`。请求体示例
（等价于
`PUT /api/v1/services/billing/instances/i-1/health?healthy=false`，二者
同时出现时以请求体为准）：

```json
{"healthy": false}
```

成功返回 HTTP 200，顶层 `instance` 为修改后的完整记录：

```json
{"instance":{"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":false,"weight":20,"heartbeat_at":"2026-10-01T12:00:00Z"}}
```

批量请求体必须是单个 JSON 对象，且只接受其中的 `updates` 非空数组；每
项为对象并包含 `instance_id` 与布尔 `healthy`，`instance_id` 去掉首尾空
白后不得为空，同批不得重复：

```json
{
  "updates": [
    {"instance_id": "i-2", "healthy": true},
    {"instance_id": "i-1", "healthy": false}
  ]
}
```

请求先完整校验并确认所有目标存在，全部成立后才在一个事务里统一替换
`healthy`。成功返回 HTTP 200，`updated` 为修改条数，`instances` 严格按
请求顺序包含修改后的完整记录：

```json
{
  "updated": 2,
  "instances": [
    {"service_name":"billing","instance_id":"i-2","address":"10.0.0.9:8080","port":8080,"healthy":true,"weight":9.5,"heartbeat_at":"2025-10-01T12:05:00Z"},
    {"service_name":"billing","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":false,"weight":20,"heartbeat_at":"2026-10-01T12:00:00Z"}
  ]
}
```

服务名或实例标识为空、`healthy` 缺失或表示不合规（JSON 非布尔、查询参
数不是小写 `true`/`false`），以及批量的 `updates` 缺失、为空、不是数
组、条目不是对象、缺少 `instance_id`、`instance_id` 为空白或同批重复，
均返回 HTTP 400 `invalid_parameter` 且不修改任何记录。目标实例不存在时
（单实例或批量中的任一条目）返回 HTTP 404 `instance_not_found`，批量整
批不生效；存储读写失败返回 HTTP 503 `storage_unavailable`，事务失败不
留下部分更新。

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

### 批量发现健康实例

- `POST /api/v1/discover/batch`

一次查询多个服务，所有服务共用同一评估时刻与超时时长。请求体必须是单个
JSON 对象（数组、裸值、空请求体都返回 HTTP 400）：

```json
{
  "service_names": ["billing", "gateway"],
  "evaluate_at": "2026-10-01T12:10:00Z",
  "heartbeat_timeout": "2m"
}
```

- `service_names`：必填的非空字符串数组；每项去除首尾空白后必须非空，
  去除空白后的名称不能重复，否则返回 HTTP 400 `invalid_parameter`。
- `evaluate_at`：必填，接受 RFC3339 时间或 Unix 秒。
- `heartbeat_timeout`：必填且大于零，接受正数秒数或 `30s`、`2m` 时长文本。

处理只读取请求指定的服务：未指定的服务既不读取也不删除。失联判定与单服务
发现一致，仅当 `evaluate_at` 晚于 `heartbeat_at + heartbeat_timeout` 时判定
失联，恰好等于边界仍在线；每个服务只把 `healthy` 为 `true` 且未失联的实例
放入结果，失联实例按同一边界从对应服务删除；显式不健康但未失联的实例保留，
没有记录的服务返回空实例数组 `[]`。

请求先完成全部参数与数据校验（包括 `evaluate_at` 不得早于任一指定服务内
任一实例的 `heartbeat_at`），之后才判定失联并删除：任何校验错误都不会
创建、更新或删除实例。所有失联记录在单个事务内跨服务原子删除，存储读取或
删除失败返回 HTTP 503 `storage_unavailable`，不会留下部分删除结果。

成功返回 HTTP 200：顶层 `services` 严格按 `service_names` 原顺序排列，
并含规范化的 `evaluate_at` 和以 JSON 秒数表示的 `heartbeat_timeout`；
每项含 `service_name` 与 `instances`，实例字段沿用单服务发现，排序同样为
权重降序、权重相同按 `instance_id` 升序：

```json
{
  "services": [
    {"service_name":"billing","instances":[{"service_name":"billing","instance_id":"i-2","address":"","port":0,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:09:00Z"}]},
    {"service_name":"gateway","instances":[]}
  ],
  "evaluate_at": "2026-10-01T12:10:00Z",
  "heartbeat_timeout": 120
}
```

### 失联实例清理

- `POST /api/v1/cleanup`

参数从查询参数或 JSON 对象读取：`evaluate_at`（评估时刻，必填，接受
RFC3339 时间或 Unix 秒）、`heartbeat_timeout`（心跳超时时长，必填且大于零，
接受正数秒数或 `30s`、`2m` 时长文本）、`service_name`（可选）。省略
`service_name` 时清理全部服务；显式提供时必须非空，且只清理该服务，
其他服务的记录不受影响。

失联判定与发现入口一致：仅当 `evaluate_at` 晚于
`heartbeat_at + heartbeat_timeout` 时才删除记录，恰好等于边界以及仍在线的
记录必须保留，显式不健康但未超过时限的实例也不会被删除。请求先完成全部
校验再产生删除结果。清理完成后返回 HTTP 200：

```json
{
  "evaluate_at": "2026-10-01T12:10:00Z",
  "heartbeat_timeout": 300,
  "removed": 2,
  "instances": [
    {"service_name":"alpha","instance_id":"i-1","address":"10.0.0.8:8080","port":8080,"healthy":true,"weight":10,"heartbeat_at":"2026-10-01T12:00:00Z"},
    {"service_name":"beta","instance_id":"i-1","address":"","port":0,"healthy":false,"weight":5,"heartbeat_at":"2026-10-01T12:01:00Z"}
  ]
}
```

`heartbeat_timeout` 以 JSON 秒数表示；`removed` 为本次删除的记录数；
`instances` 为刚删除的完整实例记录，按 `service_name` 升序、
`instance_id` 升序排列；没有任何记录被删除时 `removed` 为 `0` 且
`instances` 为空数组。`evaluate_at` 缺失或无法解析、`heartbeat_timeout`
缺失或不大于零、显式 `service_name` 为空，或 `evaluate_at` 早于任一
待评估实例的 `heartbeat_at`，都返回 HTTP 400 `invalid_parameter`
且不修改任何记录；存储读写失败返回 HTTP 503 `storage_unavailable`，
单次清理不会留下部分删除结果。

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
发现请求（含批量发现）触发的失联清理由请求同步完成，仅删除请求指定服务名下严格超过时限的记录；批量发现的跨服务删除在单个事务内原子完成。
