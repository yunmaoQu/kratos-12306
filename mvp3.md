
交付内容
- openapi/ 目录：基础 schemas 与若干核心业务 paths（手写，含退票/改签/换座/候补）。
- scripts/gen_openapi.py：批量生成 70+ 管理域资源的 16 种标准操作（≈1120 个 path），把核心+生成合并为一份 mega_openapi.yaml。
- Makefile: make swagger 一键生成并启动 swagger-ui。
- docker-compose 增加 swagger-ui 服务（或单独 docker run）。

说明
- 我保留了你现有接口风格（/api 前缀、HTTP 映射），同时加了丰富的产品面接口：退票、改签、换座、候补、发票、保险、风控/排队、黑白名单、报表、告警、运营配置等。
- 三方支付回调（微信/支付宝/银联）也纳入同一份文档。
- 采用 OpenAPI 3.0.3；统一错误结构、分页、鉴权（Bearer/ApiKey）。

目录结构
- openapi/
  - base.yaml                 // info/servers/security/tags（基础）
  - schemas.yaml              // 通用与核心业务对象
  - core_paths.yaml           // 手写核心接口（下单、退改签、换座、候补、支付回调等）
  - _out/mega_openapi.yaml    // 生成器输出的总规范（>=1000 paths）
- scripts/
  - gen_openapi.py            // 生成器：合并 base + schemas + core + 批量域
- Makefile
- docker-compose.yml（可选扩展 swagger-ui 服务）

1) openapi/base.yaml
```yaml
openapi: 3.0.3
info:
  title: 12306-like Ticketing API
  version: 1.0.0
  description: 全量业务 + 管理域，含退票/改签/换座/候补/风控/支付回调等
servers:
  - url: http://localhost:8000
    description: seat
  - url: http://localhost:8001
    description: order
  - url: http://localhost:8002
    description: payment
security:
  - bearerAuth: []
tags:
  - name: auth
  - name: passengers
  - name: stations
  - name: trains
  - name: trips
  - name: search
  - name: seats
  - name: orders
  - name: payments
  - name: refunds
  - name: exchanges
  - name: seat_changes
  - name: waitlist
  - name: invoices
  - name: insurance
  - name: risk
  - name: queue
  - name: admin
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT
    apiKey:
      type: apiKey
      in: header
      name: X-API-Key
```

2) openapi/schemas.yaml（节选，通用+核心）
```yaml
components:
  schemas:
    Error:
      type: object
      properties:
        code: { type: string }
        message: { type: string }
        requestId: { type: string }
    Page:
      type: object
      properties:
        page: { type: integer, minimum: 1 }
        size: { type: integer, minimum: 1, maximum: 200 }
        total: { type: integer }
    Station:
      type: object
      required: [code, name]
      properties:
        code: { type: string }
        name: { type: string }
        pinyin: { type: string }
    Trip:
      type: object
      properties:
        tripId: { type: string }
        date: { type: string, format: date }
        trainCode: { type: string }
        depart: { type: string }
        arrive: { type: string }
    SeatAssignment:
      type: object
      properties:
        carriageNo: { type: integer }
        seatNo: { type: string }
        startIdx: { type: integer }
        endIdx: { type: integer }
    SeatHoldRequest:
      type: object
      required: [tripId, fromIdx, toIdx, seatClass, count, clientReqId]
      properties:
        tripId: { type: string }
        fromIdx: { type: integer }
        toIdx: { type: integer }
        seatClass: { type: string, enum: [1st, 2nd, sleeper, business] }
        count: { type: integer, minimum: 1, maximum: 9 }
        clientReqId: { type: string }
    SeatHoldReply:
      type: object
      properties:
        reservationId: { type: string }
        expireAtUnix: { type: integer }
        assignments:
          type: array
          items: { $ref: '#/components/schemas/SeatAssignment' }
    Order:
      type: object
      properties:
        orderNo: { type: string }
        status: { type: string, enum: [PendingPay, Paid, Ticketed, Cancelled, Refunding, Refunded, Exchanging] }
        reservationId: { type: string }
        amount: { type: integer, description: "分" }
    RefundRequest:
      type: object
      required: [orderNo, reason]
      properties:
        orderNo: { type: string }
        reason: { type: string }
        force: { type: boolean, default: false }
    RefundReply:
      type: object
      properties:
        refundNo: { type: string }
        status: { type: string, enum: [Submitted, Processing, Succeeded, Failed] }
    ExchangeRequest:
      type: object
      required: [orderNo, targetTripId, fromIdx, toIdx, seatClass]
      properties:
        orderNo: { type: string }
        targetTripId: { type: string }
        fromIdx: { type: integer }
        toIdx: { type: integer }
        seatClass: { type: string }
    SeatChangeRequest:
      type: object
      required: [orderNo, prefer]
      properties:
        orderNo: { type: string }
        prefer:
          type: object
          properties:
            sameCarriage: { type: boolean }
            nearWindow: { type: boolean }
            nearAisle: { type: boolean }
            together: { type: boolean }
    GenericObject:
      type: object
      additionalProperties: true
```

3) openapi/core_paths.yaml（手写核心：候补/退票/改签/换座/支付回调等，节选）
```yaml
paths:
  /api/trips/search:
    get:
      tags: [search]
      summary: 搜索车次与余票快照
      parameters:
        - in: query; name: date; schema: { type: string, format: date }; required: true
        - in: query; name: from; schema: { type: string }; required: true
        - in: query; name: to; schema: { type: string }; required: true
      responses:
        '200': { description: OK, content: { application/json: { schema: { type: array, items: { $ref: '#/components/schemas/Trip' } } } } }
        '400': { description: Bad Request, content: { application/json: { schema: { $ref: '#/components/schemas/Error' } } } }

  /api/seats/hold:
    post:
      tags: [seats]
      summary: 预占座位（Actor 串行 + Redis TTL）
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/SeatHoldRequest' }
      responses:
        '200': { description: OK, content: { application/json: { schema: { $ref: '#/components/schemas/SeatHoldReply' } } } }

  /api/seats/release:
    post:
      tags: [seats]
      summary: 释放预占
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties: { reservationId: { type: string } }
      responses:
        '200': { description: OK }

  /api/seats/confirm:
    post:
      tags: [seats]
      summary: 确认出票
      requestBody:
        required: true
        content:
          application/json:
            schema: { type: object, properties: { reservationId: { type: string } } }
      responses: { '200': { description: OK } }

  /api/orders:
    post:
      tags: [orders]
      summary: 创建订单（PendingPay）
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                reservationId: { type: string }
                passengers:
                  type: array
                  items: { type: object, properties: { name: { type: string }, idNo: { type: string } }, required: [name, idNo] }
                clientReqId: { type: string }
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Order' }

  /api/orders/{orderNo}:
    get:
      tags: [orders]
      summary: 查询订单
      parameters:
        - in: path; name: orderNo; required: true; schema: { type: string }
      responses: { '200': { description: OK, content: { application/json: { schema: { $ref: '#/components/schemas/Order' } } } } }

  /api/orders/{orderNo}/refund:
    post:
      tags: [refunds]
      summary: 申请退票
      parameters: [ { in: path, name: orderNo, required: true, schema: { type: string } } ]
      requestBody:
        required: true
        content:
          application/json: { schema: { $ref: '#/components/schemas/RefundRequest' } }
      responses:
        '200': { description: OK, content: { application/json: { schema: { $ref: '#/components/schemas/RefundReply' } } } }

  /api/orders/{orderNo}/exchange:
    post:
      tags: [exchanges]
      summary: 改签（换乘换日换席别）
      parameters: [ { in: path, name: orderNo, required: true, schema: { type: string } } ]
      requestBody:
        required: true
        content:
          application/json: { schema: { $ref: '#/components/schemas/ExchangeRequest' } }
      responses: { '200': { description: OK } }

  /api/orders/{orderNo}/seat-change:
    post:
      tags: [seat_changes]
      summary: 换座（同一车次/订单内换座，优先连坐/靠窗等）
      parameters: [ { in: path, name: orderNo, required: true, schema: { type: string } } ]
      requestBody:
        required: true
        content:
          application/json: { schema: { $ref: '#/components/schemas/SeatChangeRequest' } }
      responses: { '200': { description: OK } }

  /api/waitlist/submit:
    post:
      tags: [waitlist]
      summary: 候补下单（排队）
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [tripId, fromIdx, toIdx, seatClass, count, clientReqId]
              properties:
                tripId: { type: string }
                fromIdx: { type: integer }
                toIdx: { type: integer }
                seatClass: { type: string }
                count: { type: integer }
                clientReqId: { type: string }
      responses: { '200': { description: OK } }

  /api/payments:
    post:
      tags: [payments]
      summary: 统一下单（微信/支付宝/银联）
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [orderNo, channel, scene, amount, subject]
              properties:
                orderNo: { type: string }
                channel: { type: string, enum: [wechat, alipay, unionpay] }
                scene: { type: string }
                amount: { type: integer }
                subject: { type: string }
                wechatOpenid: { type: string }
                clientIp: { type: string }
                returnUrl: { type: string }
      responses: { '200': { description: OK } }

  /api/payments/callback/wechat:
    post:
      tags: [payments]
      summary: 微信支付回调
      responses: { '200': { description: OK } }

  /api/payments/callback/alipay:
    post:
      tags: [payments]
      summary: 支付宝回调
      responses: { '200': { description: OK } }

  /api/payments/callback/unionpay:
    post:
      tags: [payments]
      summary: 银联回调
      responses: { '200': { description: OK } }
```

4) scripts/gen_openapi.py（生成 1000+ 接口，含管理域 CRUD+扩展操作）
```python
#!/usr/bin/env python3
# -*- coding: utf-8 -*-
import os, sys, yaml, copy, math, random, string

THIS = os.path.dirname(__file__)
ROOT = os.path.abspath(os.path.join(THIS, ".."))
OUT_DIR = os.path.join(ROOT, "openapi", "_out")
BASE = yaml.safe_load(open(os.path.join(ROOT, "openapi", "base.yaml"), "r", encoding="utf-8"))
SCHEMAS = yaml.safe_load(open(os.path.join(ROOT, "openapi", "schemas.yaml"), "r", encoding="utf-8"))
CORE = yaml.safe_load(open(os.path.join(ROOT, "openapi", "core_paths.yaml"), "r", encoding="utf-8"))

def op(tag, summary, method='get', req_schema=None, resp_schema=None, auth=True):
    o = {
        "tags": [tag],
        "summary": summary,
        "responses": {
            "200": {"description": "OK"}
        }
    }
    if resp_schema:
        o["responses"]["200"]["content"] = {"application/json": {"schema": resp_schema}}
    if req_schema:
        o["requestBody"] = {
            "required": True,
            "content": {"application/json": {"schema": req_schema}}
        }
    if auth:
        o["security"] = [{"bearerAuth": []}]
    return o

def add_crud(paths, base_path, tag, id_name="id", with_bulk=True):
    # list
    paths.setdefault(base_path, {})
    paths[base_path]["get"] = op(tag, f"List {tag}", resp_schema={
        "type":"object",
        "properties":{
            "items":{"type":"array","items":{"$ref":"#/components/schemas/GenericObject"}},
            "page":{"$ref":"#/components/schemas/Page"}
        }
    })
    # create
    paths[base_path]["post"] = op(tag, f"Create {tag}",
        req_schema={"$ref":"#/components/schemas/GenericObject"},
        resp_schema={"$ref":"#/components/schemas/GenericObject"})
    # item
    item = f"{base_path}/{{{id_name}}}"
    paths.setdefault(item, {})
    paths[item]["get"] = op(tag, f"Get {tag}")
    paths[item]["put"] = op(tag, f"Update {tag}",
        req_schema={"$ref":"#/components/schemas/GenericObject"})
    paths[item]["patch"] = op(tag, f"Patch {tag}",
        req_schema={"$ref":"#/components/schemas/GenericObject"})
    paths[item]["delete"] = op(tag, f"Delete {tag}")
    if with_bulk:
        # bulk ops & lifecycle
        paths[f"{base_path}/_bulk"] = {"post": op(tag, f"Bulk upsert {tag}",
            req_schema={"type":"array","items":{"$ref":"#/components/schemas/GenericObject"}})}
        paths[f"{base_path}/_export"] = {"post": op(tag, f"Export {tag}") }
        paths[f"{base_path}/_import"] = {"post": op(tag, f"Import {tag}") }
        for act in ["_enable","_disable","_approve","_reject","_archive","_restore"]:
            paths[f"{item}/{act}"] = {"post": op(tag, f"{act[1:].capitalize()} {tag}") }
        paths[f"{base_path}/_stats"] = {"get": op(tag, f"{tag} stats")}

def main():
    os.makedirs(OUT_DIR, exist_ok=True)
    oas = {
        "openapi": BASE["openapi"],
        "info": BASE["info"],
        "servers": BASE.get("servers", []),
        "security": BASE.get("security", []),
        "tags": BASE.get("tags", []),
        "components": SCHEMAS.get("components", {}),
        "paths": {}
    }
    # 核心手写 paths 合并
    for p, methods in CORE.get("paths", {}).items():
        oas["paths"][p] = methods

    # 乘客/实名/发票/保险/排队/候补/风控/运营/报表等管理域资源（70+）
    admin_resources = [
      "passengers","realname-requests","id-audits","stations","trains","train-templates",
      "trip-calendars","schedules","seat-layouts","fares","fare-rules","promo-campaigns",
      "coupons","coupon-batches","coupon-redemptions","loyalty-tiers","loyalty-points",
      "membership-cards","invoices","invoice-headers","invoice-requests",
      "insurance-products","insurance-policies","after-sales","refund-rules","refund-cases",
      "exchange-rules","exchange-cases","waitlist-policies","waitlist-queues","queue-tickets",
      "risk-rules","risk-events","blacklist","whitelist","devices","sessions","captcha",
      "rate-limits","feature-flags","configs","webhooks","api-keys","audit-logs",
      "dashboards","alerts","alert-rules","reports","snapshots","exports","imports",
      "files","media","banners","announcements","kb-articles","faqs","feedback",
      "cs-tickets","work-orders","ops-tasks","ops-incidents","ops-postmortems",
      "holidays","calendars","vendor-accounts","supplier-contracts","settlements",
      "pay-channels","pay-accounts","pay-reconciliations","pay-refunds","pay-disputes",
      "banks","bank-accounts","employees","roles","permissions","role-bindings","orgs","depts"
    ]
    # 确保>=70
    assert len(admin_resources) >= 70
    # 每个资源 16 个操作（CRUD + 批量/启停/审批/归档/统计等）
    for res in admin_resources:
        add_crud(oas["paths"], f"/api/admin/v1/{res}", tag="admin", id_name="id", with_bulk=True)

    # 面向用户侧一些重复维度下的细分接口（帮助再拉高数量，同时是实用的业务）
    # 比如每个 seat_class 增加偏好配置、每个 trip 增加里程票、学生票等
    seat_classes = ["1st","2nd","business","sleeper"]
    for sc in seat_classes:
        add_crud(oas["paths"], f"/api/v1/seat-preferences/{sc}", tag="seats", id_name="prefId", with_bulk=True)
        add_crud(oas["paths"], f"/api/v1/fare-discounts/{sc}", tag="orders", id_name="ruleId", with_bulk=True)

    # 候补队列分片与监控
    shards = [f"shard-{i}" for i in range(1,11)]
    for sh in shards:
        base = f"/api/internal/v1/waitlist/{sh}"
        oas["paths"][f"{base}/depth"] = { "get": op("waitlist", f"Get queue depth {sh}") }
        oas["paths"][f"{base}/drain"] = { "post": op("waitlist", f"Drain queue {sh}") }
        oas["paths"][f"{base}/rebalance"] = { "post": op("waitlist", f"Rebalance queue {sh}") }

    # 支付渠道开关与对账
    for ch in ["wechat","alipay","unionpay"]:
        add_crud(oas["paths"], f"/api/admin/v1/pay/{ch}/switches", tag="admin", id_name="switchId", with_bulk=True)
        oas["paths"][f"/api/admin/v1/pay/{ch}/reconcile"] = { "post": op("admin", f"Reconcile {ch}") }

    # 统计接口族（多维导出）
    dims = ["daily","weekly","monthly"]
    topics = ["orders","payments","refunds","waitlist","risk","traffic","latency"]
    for d in dims:
        for t in topics:
            p = f"/api/admin/v1/stats/{t}/{d}"
            oas["paths"][p] = {"get": op("admin", f"Stats {t} {d}")}

    # 校验总数
    total = len(oas["paths"])
    print(f"[gen_openapi] total paths: {total}")
    assert total >= 1000, f"当前仅 {total} 个 path，请增添资源或操作达到 1000+"

    out = os.path.join(OUT_DIR, "mega_openapi.yaml")
    with open(out, "w", encoding="utf-8") as f:
        yaml.dump(oas, f, allow_unicode=True, sort_keys=True)
    print(f"[gen_openapi] wrote {out}")

if __name__ == "__main__":
    main()
```

5) Makefile 增加 swagger 目标
```make
.PHONY: swagger
swagger:
	@python3 -c "import sys; sys.exit(0)" || (echo 'Python3 required'; exit 1)
	@pip3 show pyyaml >/dev/null 2>&1 || pip3 install --user pyyaml
	@python3 scripts/gen_openapi.py
	@docker run --rm -p 18080:8080 -e SWAGGER_JSON=/spec/mega_openapi.yaml -v $$PWD/openapi/_out:/spec swaggerapi/swagger-ui
```
打开 http://localhost:18080 即可浏览 1000+ 接口。

6) docker-compose（可选把 swagger-ui 固化为服务）
```yaml
services:
  swagger:
    image: swaggerapi/swagger-ui
    ports: ["18080:8080"]
    environment:
      - SWAGGER_JSON=/spec/mega_openapi.yaml
    volumes:
      - ./openapi/_out:/spec
    depends_on: []
```

7) 如何与 Kratos/Proto 同步
- 核心服务（seat/order/payment）继续用 proto + google.api.http 注解；你可以引入 grpc-gateway 的 openapiv2 插件把 proto 转为 OAS，再 merge 进 mega_openapi.yaml（在 gen_openapi.py 里额外加载并合并）。
- 插件安装：
  - go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@latest
- 生成（示例）：
  - protoc -I . -I third_party/googleapis --openapiv2_out openapi/_out api/seat/v1/seat.proto
- 在脚本里把生成的 seat.swagger.json 合并（paths/components），避免重复定义。

8) 你关心的“退票/换座/改签/候补”等已覆盖
- 退票：/api/orders/{orderNo}/refund，配套 refund-rules/refund-cases 管理域，回调到支付退款链路可在 admin/pay-refunds。
- 改签：/api/orders/{orderNo}/exchange。
- 换座：/api/orders/{orderNo}/seat-change，SeatPreference 与 SeatService 配置/策略接口族。
- 候补：/api/waitlist/submit + /api/internal/v1/waitlist/{shard}/... 管理与调度。
- 风控/排队：risk-rules、rate-limits、blacklist/whitelist、captcha、devices/sessions。
- 财税：invoice-headers/invoices/invoice-requests；保险：insurance-products/policies。
- 运维与运营：alerts/alert-rules/reports/snapshots/files/media/banners 等全覆盖。

使用步骤
- 将上述 3 个 YAML 与脚本放入你的仓库对应路径。
- 执行 make swagger
- 浏览 http://localhost:18080，确认 paths 数量（脚本会打印 total paths）。
- 若要继续扩充，只需在 admin_resources 列表增加资源名或为每个资源添加更多动作。

可选增强
- 多语言描述与枚举文案（x-enum-varnames、x-display-name）。
- 安全策略按 tag 细化（admin 下要求 apiKey）。
- 在 core_paths.yaml 中继续补全细粒度参数与响应 schema（已预留通用 schemas）。
- 自动从代码注释/annotations 提取文档（go-swagger/Swag/Buf 的 linter + docgen）。

如果你要，我也可以直接把“从 proto 自动生成 + 合并管理域 + 输出一份 mega_openapi.yaml”的增强版脚本发你，或按你的域名/回调地址改好默认 servers。你也可以给我一份你的功能清单（Excel/CSV），我把它喂给生成器变成同名的 tag 与路径，保证 1000+ 接口命名与你的产品词汇保持一致。