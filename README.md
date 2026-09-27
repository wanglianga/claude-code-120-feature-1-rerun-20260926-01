# 海鲜预制菜工厂 · 批次解冻加工、解冻超时锁批与召回服务

基于 **Go 1.23 + PostgreSQL 16 + Redis 7** 的海鲜预制菜（虾仁 / 扇贝 / 鱿鱼 / 鱼柳）工厂
批次解冻加工、**解冻超时锁批（复测/降级/半成品隔离/返工）**、全链追溯与召回处置系统。覆盖「采购入库 → 仓储温控 →
解冻加工（超时自动锁批、品控复测、降级/隔离/返工）→ 留样检测 → 渠道发货（降级品限渠道）→
品控事件 → 精准召回 → 退货三态处置 → 客服赔付 / 供应商追偿 → 监管抽样」全流程。

---

## 原始需求

实现海鲜预制菜工厂批次解冻加工与召回服务，可使用 Go、PostgreSQL 和 Redis。工厂接收虾仁、扇贝、鱿鱼和鱼柳冻品时，服务记录供应商、捕捞或生产日期、入库温度、检疫证明、箱号和库位；生产计划下发后，解冻间记录解冻开始时间、中心温度、用水批次和操作人，加工线继续记录清洗、切配、调味、称重、包装、金检、留样和成品冷库。若出现原料温度超标、解冻超时、调味料批次用错、金检报警、包装漏气、渠道反馈异味、直播间集中退货或监管要求召回，服务要把采购入库、仓储温控、生产记录、品控结论、渠道反馈、客服赔付和财务追偿关联到原料批次和成品批次。召回时，系统需要定位已发货渠道、库存、已售订单和留样检测结果，生成停售、退货、赔偿和供应商追责材料。服务还要支持多条生产线并行、渠道定制包装、冷库盘点、供应商扣款和监管抽样。工厂管理者需要按原料批次看到它进入哪些成品、发往哪些渠道、是否留样合格，召回时才能精准到箱和订单。退货回厂后还要区分可复检、可返工和必须销毁的库存，避免召回后库存状态失真。

### 本次新增需求：解冻超时锁批

解冻间发现某批原料（如鱿鱼）解冻超过允许时间（限值 240 分钟）时：

1. 服务**自动锁批**并生成「解冻超时」品控事件，**暂停该批进入加工线**（解冻/加工/完工全链路拦截），
   并通知品控**复测中心温度与感官状态**；
2. 复测结论三选一：**合格放行**（恢复排产）/ **降级使用** / **不合格拒收**（原料销毁）；
3. **降级使用**时，成品标签（`label_text`）、销售渠道（默认仅餐饮经销 CH04，发货强校验）和成本核算
   （差价损失独立成本中心 `downgrade_costs`）都要**单独标记**；
4. 锁批期间**生产排程自动避开**该原料（`/schedule-materials` 返回可用/排除两张清单）；
5. 若该批**已进入半成品**，登记**隔离库位、检测结果、是否允许返工**（检测不合格强制不可返工）；
6. **返工成品**生成新批次（继承降级标签与渠道限制、`rework_of_finished_id` 溯源原批次、继承原料链路），
   并**重新生成留样记录**（SP-RW 前缀），返工费单列成本。

---

## 一、快速启动（Docker 一键部署）

前置：已安装 Docker 与 Docker Compose（宿主 daemon 通过 `DOCKER_HOST` 提供亦可）。

```bash
# 1) 准备环境变量（可直接复制示例）
cp .env.example .env
#    .env 中 CC_PUBLISH_PORT 为宿主机发布端口，避免并行任务端口冲突

# 2) 一键启动 app + PostgreSQL + Redis
docker compose up -d --build

# 3) 查看健康状态
docker compose ps
curl -fsS http://localhost:${CC_PUBLISH_PORT:-3120}/healthz
# => {"status":"ok"}

# 4) 浏览器访问（实际映射端口以命令输出为准）
#    http://<宿主IP>:3120
docker compose port app 8080

# 5) 验证结束，释放资源
docker compose down
```

- 仅 **app 服务**发布宿主机端口（`${CC_PUBLISH_PORT}:8080`）；PostgreSQL / Redis
  **不发布宿主端口**，只在 compose 内网通过服务名 `db` / `redis` 互访。
- 数据库表结构由服务启动时自动迁移（schema 内嵌于二进制），并幂等写入演示数据，
  重复启动不会重复播种。

**验证方式 = 宿主 `docker compose up -d` 健康 + 浏览器/接口关键业务流走通**（非本地裸跑）。

---

## 附：本地开发与裸跑验证（无 Docker 时）

需要 Go 1.23+、PostgreSQL 14+、Redis 6+。

```bash
# 1) 启动依赖（示例：本机 PostgreSQL 建库）
createdb seafood
redis-server --daemonize yes

# 2) 配置并运行（schema 自动迁移 + 幂等播种）
export PGHOST=127.0.0.1 PGPORT=5432 PGUSER=seafood PGPASSWORD=seafood_pw PGNAME=seafood
export REDIS_ADDR=127.0.0.1:6379 APP_PORT=8080
go run ./cmd/server

# 3) 健康检查与登录
curl -fsS http://localhost:8080/healthz
curl -s -c /tmp/ck -X POST http://localhost:8080/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"quality","password":"quality123"}'
```

前端为内嵌静态资源（`internal/api/web`），改动后重启服务即生效，无构建步骤。

### 本工程实际执行过的验证（2026-09-26）

> 验证环境无 Docker、无外网，仅 Go 1.19 / PostgreSQL 15 / Redis，故采用等价的本地裸跑验证；
> 交付的 Docker 镜像构建链（`golang:1.23-alpine`）未在该沙箱执行，需在有 Docker 的宿主按第一节验证。

1. `gofmt` 全部 Go 文件无格式问题，`node --check app.js` 前端语法通过；
2. 在隔离副本中仅对第三方依赖 pgx v5.7.2 的 2 处 `errors.Join`（Go 1.20+ API）打本地兼容 shim
   后，用 Go 1.19 **真实编译通过并启动**（工程本身 `go.mod` 仍声明 `go 1.23`，未做任何降级修改）；
3. PostgreSQL 15 + Redis 全新实例上完成 schema 迁移与幂等播种；
4. 运行 **43 项端到端断言全部通过（PASS=43 FAIL=0）**，覆盖：
   - 种子锁批单 `LK20260920-01` 状态/关联半成品/305 分钟超时；
   - 排程自动避开活动锁批与温控锁定原料；
   - 锁批期间加工（409）与完工（409）拦截；
   - 复测降级：原料恢复、事件自动闭环、标签/渠道回写；
   - 半成品隔离：批次与库存转 quarantined、150kg 隔离量；
   - 返工：新成品 `FG20260926-RW01`、SP-RW 重新留样、降级标记、返工费 1232.5、原批次 reworked、溯源链路；
   - 降级品发 CH03 直播 409 被拒、发 CH04 餐饮 201 放行；
   - 降级原料再完工自动落标、差价损失 (60−28)×40=1280、成本台账 2 条；
   - 超时→复测合格放行后可重新解冻；超时→复测不合格原料转 destroyed；拒收锁批禁止隔离登记；
5. 服务重启再次播种，锁批单与原料均不重复（幂等）；
6. 原有功能回归通过：`EV20260903-01` 召回圈定/材料生成、正常品 CH01 发货、冷库盘点、原料正向追溯（含新增 `thaw_locks` 段）。

---

## 二、测试账号（逐角色：用户名 / 密码 / 权限）

| 用户名 | 密码 | 角色 | 权限范围 |
| --- | --- | --- | --- |
| `admin` | `admin123` | 系统管理员 | 全部功能与全部写操作 |
| `manager` | `manager123` | 工厂经理 | 全部业务读写（召回、退货判定、财务确认） |
| `quality` | `quality123` | 品控主管 | 品控事件、留样检测、发起召回、退货判定、监管抽样、渠道反馈 |
| `warehouse` | `ware123` | 冷库仓管 | 冻品入库、补测温、发货出库、退货登记、冷库盘点 |
| `production` | `prod123` | 产线班长 | 下发计划、解冻记录、加工七步、完工入冷库、发货、订单 |
| `finance` | `finance123` | 财务专员 | 消费者赔付支付、供应商扣款确认/扣款 |

登录态使用 Redis 会话（HttpOnly Cookie，有效期 8 小时）。越权调用返回 `403`。

---

## 三、内置演示数据

- 供应商：`S001 荣成远洋`（虾仁）、`S002 大连獐子岛`（扇贝）、`S003 舟山鱿鱼合作社`（鱿鱼）、`S004 北海鱼柳`（鱼柳）。
- 原料批次：
  - `RM20260901` 虾仁，入库 -18.2℃ 正常；
  - `RM20260902` 扇贝 -17.8℃；
  - **`RM20260903` 鱿鱼，入库 -15.1℃ 超过冻品限值 -18℃，自动锁定**，且已存在开放事件 `EV20260903-01`；
  - `RM20260904` 鱼柳 -18.5℃。
- 两条生产线并行：`L1 一号解冻加工线`、`L2 二号解冻加工线`；调味料批次 `T001 蒜香`、`T002 麻辣`。
- 计划/成品：
  - `PP20260910-01 → FG20260912-01 蒜香虾仁`（直播间 250g 彩包，留样 SP-001 合格）；
  - `PP20260910-02 → FG20260912-02 麻辣鱿鱼花`（商超 500g 简包，使用问题鱿鱼原料，解冻 290 分钟已超时，留样 SP-002 合格）。
- 渠道：`CH01 盒马(商超)`、`CH02 天猫(电商)`、`CH03 抖音直播间`、`CH04 餐饮经销`。
- 发货单 `SH20260913-01/02`，已售订单 `OD20260914-001/002/003、OD20260915-010`（精确到箱号）。
- 渠道反馈：直播间就 `FG20260912-02` 反馈异味。
- 冷库库存：FG20260912-01 余 115kg、FG20260912-02 余 100kg。
- **解冻超时锁批场景**：鱿鱼原料 `RM20260920`（入库 -18.3℃ 正常）在计划 `PP20260925-01`
  解冻 **305 分钟超时**（06:00→11:05），自动生成事件 `EV20260920-01` 与活动锁批单
  `LK20260920-01`，原料锁定、加工线暂停；该批已完成清洗/切配/调味形成半成品
  `FG20260925-S1`（150kg，待包装），等待复测 → 隔离 → 返工闭环。

### 建议演示路径 A：解冻超时锁批闭环（本次新增）

1. 「**解冻锁批**」页：查看 `LK20260920-01`（305 分钟超时、关联半成品），下方「生产排程物料」中
   `RM20260920` 自动进入「排程自动避开」清单；
2. 点「**品控复测**」：录入复测中心温度与感官状态（色泽/气味/弹性），结论选 **降级使用**，
   勾选允许返工、确认降级标签文案与限定渠道（默认 CH04 餐饮经销）；
3. 点「**半成品隔离**」：填隔离库位、检测结果选合格（选不合格时系统强制不可返工）；
4. 点「**返工产出**」：填新成品批号（如 `FG20260926-RW01`）→ 原半成品转返工、新成品继承降级标签
   与渠道限制并溯源原批次、**重新生成 SP-RW 留样**、返工费进入独立成本台账；
5. 「渠道履约」尝试把降级品发往 CH03 直播间会被 **409 拦截**，发往 CH04 餐饮经销放行；
6. 降级原料再次解冻完工时，成品**自动打降级标签、继承渠道限制**并按 正常价−降级价 自动核算差价损失，
   在「解冻锁批」页底部成本台账可查；复测合格/拒收的原料则分别恢复排产或转销毁。

### 建议演示路径 B：端到端召回闭环（原有）

1. 「原料入库」→ 打开 `RM20260903` 的**正向追溯**：可见它进入成品 `FG20260912-02`、发往直播渠道、已售订单与留样结果。
2. 「品控事件」→ 对 `EV20260903-01` **发起召回**：系统自动圈定关联原料/成品、锁定批次、隔离在库库存、标记已发货渠道与已售订单（精确到箱），并一次性生成**停售通知 / 退货指令 / 消费者赔偿 / 供应商追责**四类材料，同时生成赔付与供应商扣款台账。
3. 「召回管理」→ 打开召回档案查看 9 个板块与全部材料。
4. 「退货处置」→ 登记退货回厂，分别判定 **可复检 / 可返工 / 必须销毁**，观察冷库库存按状态独立流转。
5. 「财务追偿」→ 支付消费者赔付、确认供应商扣款。
6. 「监管抽样」→ 登记抽样并录入「不合格」，系统自动生成「监管要求召回」事件，可再次发起召回。

---

## 四、功能与业务规则映射

| 需求点 | 实现 |
| --- | --- |
| 冻品接收记录供应商/捕捞生产日期/入库温度/检疫证明/箱号/库位 | 原料批次入库登记；入库温度 > -18℃ 自动锁定并写温控报警 |
| 仓储温控 | 随时补测温，超标自动锁定原料批次 |
| 解冻记录（开始时间/中心温度/用水批次/操作人） | 解冻记录；**超过 240 分钟自动生成「解冻超时」事件并自动锁批**（数据库生成列计算时长） |
| **解冻超时自动锁批** | `thaw_locks` 锁批单：原料立即锁定、加工线暂停（解冻/加工/完工全链路 409 拦截）、排程自动避开 |
| **品控复测（中心温度+感官）** | 复测三结论：合格放行（恢复在库/排产）/ 降级使用 / 不合格拒收（原料转销毁）；超时事件自动闭环 |
| **降级使用三单独标记** | 成品标签 `label_text`、限定销售渠道 `channel_restriction`（发货强校验，默认仅 CH04）、独立成本中心 `downgrade_costs`（差价损失/返工费） |
| **已入半成品隔离** | 登记隔离库位、检测结果、是否允许返工；半成品与库存转 quarantined；检测不合格强制不可返工 |
| **返工成品** | 新成品批次继承降级标签/渠道限制、`rework_of_finished_id` 溯源原批次、继承原料链路、**重新生成 SP-RW 留样**、返工费单列 |
| **排程自动避开** | `GET /schedule-materials`：活动锁批/温控锁定/已耗用/召回/销毁原料进「排除清单」并给出原因 |
| 清洗/切配/调味/称重/包装/金检/留样/成品冷库 | 加工七步留痕；调味批次与计划指定不一致→「调味料用错」；金检不合格→「金检报警」；包装不合格→「包装漏气」 |
| 多条生产线并行 | 生产线档案，计划绑定产线，L1/L2 并行 |
| 渠道定制包装 | 计划与成品批次记录包装规格（如直播间 250g 彩包、商超 500g 简包） |
| 原料 ↔ 成品双向批次关联 | `batch_links` 支持正反向追溯（一料多品、一品多料） |
| 按原料批次看成品/渠道/留样 | 原料正向追溯视图 |
| 召回定位渠道/库存/订单/留样 | 召回事务一次圈定：发货单、在库库存、已售订单（精确到箱号）、留样结果 |
| 停售/退货/赔偿/供应商追责材料 | 召回自动生成四类 `recall_actions` 文书（含编号、抬头、正文、金额） |
| 客服赔付 / 财务追偿 | `customer_compensations`（支付后订单转已赔付）、`supplier_deductions`（待确认→已确认→已扣款） |
| 退货回厂区分可复检/可返工/必须销毁 | 三态处置并同步库存：隔离待判 / 返工区 / 销毁区，避免库存状态失真 |
| 冷库盘点 | 按库位快照系统库存生成盘点单与明细 |
| 监管抽样 | 原料/成品抽样登记，**抽检不合格自动生成监管召回事件** |
| 渠道反馈异味 / 直播集中退货 | 反馈登记自动生成对应等级品控事件 |

库存状态机：成品库存 `available → quarantined（召回隔离 / 解冻超时半成品隔离）→ reworked / destroyed（退货处置 / 锁批返工）`，
账面可用量与隔离/返工/销毁量分开统计，召回隔离与锁批隔离都不会冲销可用量数字造成失真。
成品品质标记：`label_mark = normal / downgraded`（降级品标签文案 + 渠道限制 + 返工溯源单独存储，与正常品分账分渠道）。

---

## 五、HTTP API 一览（均在 `/api` 前缀下，除登录外需会话）

- 鉴权：`POST /auth/login`、`POST /auth/logout`、`GET /auth/me`
- 总览/档案：`GET /dashboard`、`/suppliers`、`/lines`、`/channels`、`/seasonings`
- 原料：`GET/POST /raw-batches`、`POST /raw-batches/{batchNo}/temp-logs`、`GET /raw-batches/{batchNo}/trace`
- 生产：`GET/POST /plans`、`POST /plans/{id}/thaw`、`POST /plans/{id}/process`、`POST /plans/{id}/finish`
- 成品：`GET /finished-batches`、`GET /finished-batches/{batchNo}/trace`、`POST /finished-batches/{batchNo}/sample`
- 渠道：`GET/POST /shipments`、`GET/POST /orders`、`GET/POST /feedback`
- 品控/召回：`GET/POST /events`、`GET/POST /recalls`、`GET /recalls/{id}`、`POST /recalls/{id}/close`
- 解冻超时锁批：`GET /thaw-locks`、`GET /thaw-locks/{id}`、
  `POST /thaw-locks/{id}/retest`（复测：qualified/downgrade/unqualified）、
  `POST /thaw-locks/{id}/isolate`（半成品隔离库位/检测/返工许可）、
  `POST /thaw-locks/{id}/rework`（返工新成品+重新留样）、
  `GET /schedule-materials`（排程可用/排除清单）、`GET /downgrade-costs`（降级/返工成本台账）
- 退货：`GET/POST /returns`、`POST /returns/{id}/dispose`
- 库存：`GET /inventory`、`GET/POST /stocktakes`
- 财务：`GET/POST /deductions`、`POST /deductions/{id}/status`、`GET/POST /compensations`、`POST /compensations/{id}/pay`
- 监管：`GET/POST /regulatory-samplings`、`POST /regulatory-samplings/{id}/result`

快速接口自测：

```bash
BASE=http://localhost:${CC_PUBLISH_PORT:-3120}
curl -s -c /tmp/ck -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"manager","password":"manager123"}'
curl -s -b /tmp/ck $BASE/api/raw-batches | head -c 400
curl -s -b /tmp/ck $BASE/api/raw-batches/RM20260920/trace | head -c 400
# 解冻超时锁批：清单 / 排程避开 / 复测降级 / 半成品隔离 / 返工
curl -s -b /tmp/ck $BASE/api/thaw-locks | head -c 400
curl -s -b /tmp/ck $BASE/api/schedule-materials | head -c 400
# 品控账号复测：
# curl -s -b /tmp/ckq -X POST $BASE/api/thaw-locks/1/retest -H 'Content-Type: application/json' \
#   -d '{"center_temp_c":4.6,"sensory":"色泽正常轻微氨味弹性尚可","result":"downgrade","allow_rework":true,"channel_restriction":"CH04"}'
```

---

## 六、工程结构

```
.
├── cmd/server/main.go          # 入口：迁移、播种、启动 HTTP
├── internal/
│   ├── db/schema.sql           # 全部表结构（启动自动迁移，ALTER 幂等）
│   ├── db/db.go / seed.go      # 连接重试 / 幂等演示数据（含解冻超时锁批场景）
│   └── api/                    # chi 路由、Redis 会话、各业务处理器、内嵌前端
│       ├── handlers_lock.go    # 解冻超时锁批：复测/隔离/返工/排程/降级成本
│       └── web/                # 单页前端（原生 HTML/CSS/JS，无构建步骤）
├── Dockerfile                  # 多阶段构建，非 root 用户 + HEALTHCHECK
├── docker-compose.yml          # app + postgres + redis，仅发布 app 端口
└── .env.example
```

安全与运维要点：多阶段 Dockerfile（golang:1.23-alpine 构建，alpine:3.20 运行）；
运行用户为非 root 的 `appuser`(uid 10001)；镜像内置 `HEALTHCHECK`；compose 中
PostgreSQL/Redis 均有 healthcheck，app 依赖两者健康后再启动；PostgreSQL 数据持久化到命名卷。
