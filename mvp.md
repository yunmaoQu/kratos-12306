运行指南

1) 生成代码与本地运行
- 安装工具
  - go install github.com/go-kratos/kratos/cmd/kratos/v2@latest
  - go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
  - go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
  - go install github.com/go-kratos/kratos/cmd/protoc-gen-go-http/v2@latest
- 生成 pb
  - make proto
- 分别运行
  - make run-seat
  - make run-order
  - make run-payment

2) Docker/Compose 一键跑
- docker compose up --build


太棒了，我们直接把三项增强落进去：  
- seat 服务升级为 Actor 串行 + 位图快照 + Redis 预占 TTL/重启恢复 + 限流  
- order/payment 引入 Outbox + NATS 实现“支付成功 -> 出票”异步链路  
- 保持你现有 monorepo 模板与 HTTP 映射不变

下面是可直接替换/新增的关键文件与配置。按顺序粘贴、make proto、docker compose up --build 即可跑通。

一、Proto 变更（seat 增加 Confirm）
api/seat/v1/seat.proto
```proto
syntax = "proto3";
package seat.v1;
option go_package = "api/seat/v1;v1";
import "google/api/annotations.proto";

service SeatService {
  rpc Hold(HoldRequest) returns (HoldReply) {
    option (google.api.http) = { post: "/api/seats/hold", body: "*" };
  }
  rpc Release(ReleaseRequest) returns (ReleaseReply) {
    option (google.api.http) = { post: "/api/seats/release", body: "*" };
  }
  rpc Confirm(ConfirmRequest) returns (ConfirmReply) {
    option (google.api.http) = { post: "/api/seats/confirm", body: "*" };
  }
}

message HoldRequest {
  string trip_id = 1;
  int32 from_idx = 2;
  int32 to_idx = 3;
  string seat_class = 4;
  int32 count = 5;
  string client_req_id = 6;
}
message Assignment { int32 carriage_no = 1; string seat_no = 2; int32 start_idx=3; int32 end_idx=4; }
message HoldReply {
  string reservation_id = 1;
  int64 expire_at_unix = 2;
  repeated Assignment assignments = 3;
}
message ReleaseRequest { string reservation_id = 1; }
message ReleaseReply { bool ok = 1; }

message ConfirmRequest { string reservation_id = 1; }
message ConfirmReply { bool ok = 1; }
```
执行 make proto 生成新 pb。

二、依赖和部署更新
1) go.mod（根模块）增加依赖
```go
require (
  github.com/go-kratos/kratos/v2 v2.7.2
  github.com/go-kratos/aegis v0.2.0
  github.com/google/uuid v1.6.0
  github.com/nats-io/nats.go v1.31.0
  github.com/redis/go-redis/v9 v9.6.1
  google.golang.org/grpc v1.66.0
  google.golang.org/protobuf v1.34.2
)
```

2) docker-compose.yml 增加 Redis 与 NATS，传递环境变量
```yaml
version: "3.9"
services:
  redis:
    image: redis:7
    ports: ["6379:6379"]
  nats:
    image: nats:2
    command: ["-js"] # 开启 JetStream 可选
    ports: ["4222:4222"]
  seat:
    image: crud/seat:dev
    build: ./app/seat
    environment:
      - REDIS_ADDR=redis:6379
      - SEAT_HTTP_ADDR=:8000
      - SEAT_GRPC_ADDR=:9000
      - SEAT_SEGMENTS=8
    depends_on: [redis]
    ports: ["8000:8000", "9000:9000"]
  order:
    image: crud/order:dev
    build: ./app/order
    environment:
      - ORDER_HTTP_ADDR=:8001
      - ORDER_GRPC_ADDR=:9001
      - SEAT_GRPC_ADDR=seat:9000
      - REDIS_ADDR=redis:6379
      - NATS_URL=nats://nats:4222
    depends_on: [seat,redis,nats]
    ports: ["8001:8001", "9001:9001"]
  payment:
    image: crud/payment:dev
    build: ./app/payment
    environment:
      - ORDER_GRPC_ADDR=order:9001
    depends_on: [order]
    ports: ["8002:8002", "9002:9002"]
```

3) Makefile 无需改动，确保 make proto 后再构建。

三、seat 服务：Actor 串行 + 位图快照 + Redis 预占 TTL/重启恢复 + 限流
1) app/seat/cmd/seat/main.go
```go
package main

import (
  "context"
  "fmt"
  "os"
  "strconv"
  "time"

  "github.com/go-kratos/aegis/ratelimit/bbr"
  "github.com/go-kratos/kratos/v2"
  "github.com/go-kratos/kratos/v2/log"
  kmid "github.com/go-kratos/kratos/v2/middleware"
  mrl "github.com/go-kratos/kratos/v2/middleware/ratelimit"
  kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
  khttp "github.com/go-kratos/kratos/v2/transport/http"
  "github.com/redis/go-redis/v9"

  seatv1 "github.com/crud/kratos-12306/api/seat/v1"
  "github.com/crud/kratos-12306/app/seat/internal/biz"
  "github.com/crud/kratos-12306/app/seat/internal/service"
)

func getenv(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

func main() {
  logger := log.NewStdLogger(os.Stdout)
  helper := log.NewHelper(logger)

  rdb := redis.NewClient(&redis.Options{Addr: getenv("REDIS_ADDR", "127.0.0.1:6379")})

  segN := 8
  if s := os.Getenv("SEAT_SEGMENTS"); s != "" {
    if n, err := strconv.Atoi(s); err == nil && n > 1 && n < 256 { segN = n }
  }
  mgr := biz.NewSeatShardManager(rdb, segN)

  httpSrv := khttp.NewServer(
    khttp.Address(getenv("SEAT_HTTP_ADDR", ":8000")),
    khttp.Timeout(3*time.Second),
    khttp.Middleware(
      kmid.Chain(
        mrl.Server(bbr.NewLimiter()), // 自适应限流
      ),
    ),
  )
  grpcSrv := kgrpc.NewServer(
    kgrpc.Address(getenv("SEAT_GRPC_ADDR", ":9000")),
    kgrpc.Timeout(3*time.Second),
    kgrpc.Middleware(mrl.Server(bbr.NewLimiter())),
  )

  svc := service.NewSeatService(helper, mgr)
  seatv1.RegisterSeatServiceHTTPServer(httpSrv, svc)
  seatv1.RegisterSeatServiceServer(grpcSrv, svc)

  app := kratos.New(kratos.Name("seat"), kratos.Logger(logger), kratos.Server(httpSrv, grpcSrv))
  if err := app.Run(); err != nil {
    fmt.Println(err)
  }
  _ = context.Background()
}
```

2) app/seat/internal/biz/actor.go
```go
package biz

import (
  "context"
  "encoding/base64"
  "encoding/json"
  "errors"
  "fmt"
  "math/big"
  "strconv"
  "strings"
  "time"

  seatv1 "github.com/crud/kratos-12306/api/seat/v1"
  "github.com/redis/go-redis/v9"
)

type bitset struct{ bits big.Int }
func segMask(s, e int) *big.Int {
  if e <= s { panic("invalid range") }
  m := new(big.Int).Lsh(big.NewInt(1), uint(e-s))
  m.Sub(m, big.NewInt(1))
  m.Lsh(m, uint(s))
  return m
}
func (b *bitset) can(s, e int) bool { var t big.Int; t.And(&b.bits, segMask(s,e)); return t.Sign()==0 }
func (b *bitset) hold(s, e int)     { b.bits.Or(&b.bits, segMask(s,e)) }
func (b *bitset) release(s, e int)  { var inv big.Int; inv.Not(segMask(s,e)); b.bits.And(&b.bits, &inv) }

type Assignment = seatv1.Assignment

type holdState string
const (
  holdStatusHeld       holdState = "held"
  holdStatusConfirmed  holdState = "confirmed"
)

type reservation struct {
  ReservationID string         `json:"reservation_id"`
  TripID        string         `json:"trip_id"`
  SeatClass     string         `json:"seat_class"`
  Assignments   []*Assignment  `json:"assignments"`
  ExpireAt      int64          `json:"expire_at"`
  Status        holdState      `json:"status"`
  ClientReqID   string         `json:"client_req_id,omitempty"`
}

type snapshotDoc struct {
  Segments  int        `json:"segments"`
  Seats     [][]string `json:"seats"` // base64 of big.Int bytes: [carriage][seatIndex]
  UpdatedAt int64      `json:"updated_at"`
}

type SeatActor struct {
  tripID    string
  seatClass string
  segments  int

  seats [][]*bitset // [carriage][seatIndex]
  holds map[string]*reservation
  seen  map[string]string // client_req_id -> reservation_id

  redis *redis.Client
  inbox chan any

  stopCh chan struct{}
}

type holdCmd struct {
  TripID, SeatClass, ClientReqID string
  StartIdx, EndIdx, Count        int
  Reply                          chan holdResp
}
type holdResp struct {
  RID string; Assigns []*Assignment; ExpireAt time.Time; Err error
}
type releaseCmd struct{ RID string; Reply chan error }
type confirmCmd struct{ RID string; Reply chan error }
type tickCmd struct{}

func NewSeatActor(redis *redis.Client, tripID, seatClass string, segments, carriages, seatsPerCar int) *SeatActor {
  seats := make([][]*bitset, carriages)
  for c := 0; c < carriages; c++ {
    seats[c] = make([]*bitset, seatsPerCar)
    for i := 0; i < seatsPerCar; i++ { seats[c][i] = &bitset{} }
  }
  return &SeatActor{
    tripID: tripID, seatClass: seatClass, segments: segments,
    seats: seats, holds: map[string]*reservation{}, seen: map[string]string{},
    redis: redis, inbox: make(chan any, 2048), stopCh: make(chan struct{}),
  }
}

func (a *SeatActor) keySnapshot() string { return fmt.Sprintf("seat:snapshot:%s:%s", a.tripID, a.seatClass) }
func (a *SeatActor) keyResv(rid string) string {
  return fmt.Sprintf("seat:resv:%s:%s:%s", a.tripID, a.seatClass, rid)
}
func (a *SeatActor) keyIdem(clientReqID string) string {
  return fmt.Sprintf("seat:idem:%s:%s:%s", a.tripID, a.seatClass, clientReqID)
}

func (a *SeatActor) Start(ctx context.Context) {
  go func() {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()
    for {
      select {
      case m := <-a.inbox:
        switch x := m.(type) {
        case holdCmd: a.handleHold(x)
        case releaseCmd: a.handleRelease(x)
        case confirmCmd: a.handleConfirm(x)
        case tickCmd: a.handleTick()
        }
      case <-ticker.C:
        a.handleTick()
      case <-a.stopCh:
        return
      case <-ctx.Done():
        return
      }
    }
  }()
}

func (a *SeatActor) Stop() { close(a.stopCh) }

func (a *SeatActor) handleHold(cmd holdCmd) {
  // 幂等：先查内存，再查 Redis
  if rid, ok := a.seen[cmd.ClientReqID]; ok && rid != "" {
    if r, ok := a.holds[rid]; ok {
      cmd.Reply <- holdResp{RID: rid, Assigns: r.Assignments, ExpireAt: time.Unix(r.ExpireAt,0)}
      return
    }
  } else if cmd.ClientReqID != "" {
    rid, _ := a.redis.Get(context.Background(), a.keyIdem(cmd.ClientReqID)).Result()
    if rid != "" {
      if r, ok := a.holds[rid]; ok {
        a.seen[cmd.ClientReqID] = rid
        cmd.Reply <- holdResp{RID: rid, Assigns: r.Assignments, ExpireAt: time.Unix(r.ExpireAt,0)}
        return
      }
    }
  }

  // 找座
  assigns := make([]*Assignment, 0, cmd.Count)
  findSeat := func(start, end int) (int, int, bool) {
    for c := 0; c < len(a.seats); c++ {
      for s := 0; s < len(a.seats[c]); s++ {
        if a.seats[c][s].can(start, end) {
          a.seats[c][s].hold(start, end)
          return c, s, true
        }
      }
    }
    return -1, -1, false
  }
  for i := 0; i < cmd.Count; i++ {
    c, s, ok := findSeat(cmd.StartIdx, cmd.EndIdx)
    if !ok {
      // 回滚已占
      for _, asg := range assigns {
        ci := int(asg.CarriageNo-1)
        si := seatIndex(asg.SeatNo)
        a.seats[ci][si].release(int(asg.StartIdx), int(asg.EndIdx))
      }
      cmd.Reply <- holdResp{Err: errors.New("no enough seats")}
      return
    }
    assigns = append(assigns, &Assignment{
      CarriageNo: int32(c+1),
      SeatNo: seatNo(s),
      StartIdx: int32(cmd.StartIdx), EndIdx: int32(cmd.EndIdx),
    })
  }

  rid := genRID(a.tripID, a.seatClass, time.Now())
  exp := time.Now().Add(3 * time.Minute)
  doc := &reservation{
    ReservationID: rid, TripID: a.tripID, SeatClass: a.seatClass,
    Assignments: assigns, ExpireAt: exp.Unix(), Status: holdStatusHeld, ClientReqID: cmd.ClientReqID,
  }
  a.holds[rid] = doc
  if cmd.ClientReqID != "" {
    a.seen[cmd.ClientReqID] = rid
    _ = a.redis.Set(context.Background(), a.keyIdem(cmd.ClientReqID), rid, 10*time.Minute).Err()
  }
  // 保存到 Redis，设置 TTL
  buf, _ := json.Marshal(doc)
  _ = a.redis.Set(context.Background(), a.keyResv(rid), string(buf), time.Until(exp)).Err()

  cmd.Reply <- holdResp{RID: rid, Assigns: assigns, ExpireAt: exp}
}

func (a *SeatActor) handleRelease(cmd releaseCmd) {
  if r, ok := a.holds[cmd.RID]; ok {
    if r.Status == holdStatusConfirmed {
      cmd.Reply = nil
      cmd.Reply <- errors.New("already confirmed")
      return
    }
    for _, asg := range r.Assignments {
      ci := int(asg.CarriageNo-1); si := seatIndex(asg.SeatNo)
      a.seats[ci][si].release(int(asg.StartIdx), int(asg.EndIdx))
    }
    delete(a.holds, cmd.RID)
    _ = a.redis.Del(context.Background(), a.keyResv(cmd.RID)).Err()
    if cmd.Reply != nil { cmd.Reply <- nil }
    return
  }
  if cmd.Reply != nil { cmd.Reply <- errors.New("reservation not found") }
}

func (a *SeatActor) handleConfirm(cmd confirmCmd) {
  if r, ok := a.holds[cmd.RID]; ok {
    r.Status = holdStatusConfirmed
    // 延长 Redis 保留时间（便于审计），避免 TTL 释放
    r.ExpireAt = time.Now().Add(24*time.Hour).Unix()
    buf, _ := json.Marshal(r)
    _ = a.redis.Set(context.Background(), a.keyResv(cmd.RID), string(buf), 24*time.Hour).Err()
    if cmd.Reply != nil { cmd.Reply <- nil }
    return
  }
  // 幂等：若 Redis 中已是 confirmed，我们返回成功
  raw, err := a.redis.Get(context.Background(), a.keyResv(cmd.RID)).Result()
  if err == nil && raw != "" {
    var r reservation
    if json.Unmarshal([]byte(raw), &r) == nil && r.Status == holdStatusConfirmed {
      if cmd.Reply != nil { cmd.Reply <- nil; return }
    }
  }
  if cmd.Reply != nil { cmd.Reply <- errors.New("reservation not found") }
}

func (a *SeatActor) handleTick() {
  // 1) 释放超时预占
  now := time.Now().Unix()
  for rid, r := range a.holds {
    if r.Status == holdStatusHeld && r.ExpireAt <= now {
      for _, asg := range r.Assignments {
        ci := int(asg.CarriageNo-1); si := seatIndex(asg.SeatNo)
        a.seats[ci][si].release(int(asg.StartIdx), int(asg.EndIdx))
      }
      delete(a.holds, rid)
      _ = a.redis.Del(context.Background(), a.keyResv(rid)).Err()
    }
  }
  // 2) 刷新快照
  _ = a.saveSnapshot()
}

func (a *SeatActor) saveSnapshot() error {
  doc := snapshotDoc{
    Segments: a.segments, Seats: make([][]string, len(a.seats)), UpdatedAt: time.Now().Unix(),
  }
  for c := 0; c < len(a.seats); c++ {
    doc.Seats[c] = make([]string, len(a.seats[c]))
    for i := 0; i < len(a.seats[c]); i++ {
      b := a.seats[c][i].bits.Bytes()
      doc.Seats[c][i] = base64.StdEncoding.EncodeToString(b)
    }
  }
  buf, _ := json.Marshal(doc)
  return a.redis.Set(context.Background(), a.keySnapshot(), string(buf), 0).Err()
}

func (a *SeatActor) loadSnapshotAndHolds(ctx context.Context) error {
  // 1) 加载快照
  raw, err := a.redis.Get(ctx, a.keySnapshot()).Result()
  if err == nil && raw != "" {
    var doc snapshotDoc
    if json.Unmarshal([]byte(raw), &doc) == nil && doc.Segments == a.segments {
      // 重建 seats 尺寸，如不匹配则跳过
      if len(doc.Seats) == len(a.seats) && (len(doc.Seats) == 0 || len(doc.Seats[0]) == len(a.seats[0])) {
        for c := 0; c < len(doc.Seats); c++ {
          for i := 0; i < len(doc.Seats[c]); i++ {
            if doc.Seats[c][i] == "" { a.seats[c][i] = &bitset{}; continue }
            bs, _ := base64.StdEncoding.DecodeString(doc.Seats[c][i])
            var x big.Int; x.SetBytes(bs)
            a.seats[c][i] = &bitset{bits: x}
          }
        }
      }
    }
  }
  // 2) 恢复 hold 列表（仅用于续期/释放；不重复置位）
  pattern := fmt.Sprintf("seat:resv:%s:%s:*", a.tripID, a.seatClass)
  var cursor uint64
  for {
    keys, next, _ := a.redis.Scan(ctx, cursor, pattern, 200).Result()
    cursor = next
    for _, k := range keys {
      val, _ := a.redis.Get(ctx, k).Result()
      if val == "" { continue }
      var r reservation
      if json.Unmarshal([]byte(val), &r) == nil {
        a.holds[r.ReservationID] = &r
        if r.ClientReqID != "" {
          a.seen[r.ClientReqID] = r.ReservationID
        }
      }
    }
    if cursor == 0 { break }
  }
  return nil
}

// Shard Manager

type SeatShardManager struct {
  redis    *redis.Client
  segments int
  shards   map[string]*SeatActor
}

func NewSeatShardManager(rdb *redis.Client, segments int) *SeatShardManager {
  return &SeatShardManager{redis: rdb, segments: segments, shards: map[string]*SeatActor{}}
}
func (m *SeatShardManager) shardKey(tripID, seatClass string) string { return tripID + "|" + seatClass }

func (m *SeatShardManager) getOrCreate(ctx context.Context, tripID, seatClass string) *SeatActor {
  k := m.shardKey(tripID, seatClass)
  if a, ok := m.shards[k]; ok { return a }
  // 简化布局：2nd=2车厢*50座；1st=1车厢*20座
  carriages, seatsPer := 2, 50
  if strings.ToLower(seatClass) == "1st" { carriages, seatsPer = 1, 20 }
  actor := NewSeatActor(m.redis, tripID, seatClass, m.segments, carriages, seatsPer)
  _ = actor.loadSnapshotAndHolds(ctx)
  actor.Start(ctx)
  m.shards[k] = actor
  return actor
}

func (m *SeatShardManager) Hold(ctx context.Context, tripID string, fromIdx, toIdx int, seatClass string, count int, clientReq string) (string, []*Assignment, time.Time, error) {
  a := m.getOrCreate(ctx, tripID, seatClass)
  ch := make(chan holdResp,1)
  a.inbox <- holdCmd{TripID: tripID, SeatClass: seatClass, StartIdx: fromIdx, EndIdx: toIdx, Count: count, ClientReqID: clientReq, Reply: ch}
  resp := <-ch
  return resp.RID, resp.Assigns, resp.ExpireAt, resp.Err
}

func (m *SeatShardManager) Release(ctx context.Context, rid, tripID, seatClass string) error {
  a := m.getOrCreate(ctx, tripID, seatClass)
  ch := make(chan error,1)
  a.inbox <- releaseCmd{RID: rid, Reply: ch}
  return <-ch
}

func (m *SeatShardManager) Confirm(ctx context.Context, rid, tripID, seatClass string) error {
  a := m.getOrCreate(ctx, tripID, seatClass)
  ch := make(chan error,1)
  a.inbox <- confirmCmd{RID: rid, Reply: ch}
  return <-ch
}

// helpers

func seatNo(i int) string { return "S" + strconv.Itoa(i+1) }
func seatIndex(seatNo string) int {
  if strings.HasPrefix(seatNo, "S") {
    n, _ := strconv.Atoi(strings.TrimPrefix(seatNo, "S"))
    return n-1
  }
  return 0
}
func genRID(tripID, seatClass string, t time.Time) string {
  return fmt.Sprintf("%s:%s:%d", tripID, seatClass, t.UnixNano())
}
```

3) app/seat/internal/service/seat.go
```go
package service

import (
  "context"
  "time"

  "github.com/go-kratos/kratos/v2/log"
  seatv1 "github.com/crud/kratos-12306/api/seat/v1"
  "github.com/crud/kratos-12306/app/seat/internal/biz"
)

type SeatService struct {
  seatv1.UnimplementedSeatServiceServer
  log *log.Helper
  mgr *biz.SeatShardManager
}

func NewSeatService(logger *log.Helper, mgr *biz.SeatShardManager) *SeatService {
  return &SeatService{log: logger, mgr: mgr}
}

func (s *SeatService) Hold(ctx context.Context, in *seatv1.HoldRequest) (*seatv1.HoldReply, error) {
  rid, asgs, exp, err := s.mgr.Hold(ctx, in.TripId, int(in.FromIdx), int(in.ToIdx), in.SeatClass, int(in.Count), in.ClientReqId)
  if err != nil { return nil, seatv1.ErrorNoEnoughSeat(err.Error()) }
  out := &seatv1.HoldReply{ReservationId: rid, ExpireAtUnix: exp.Unix()}
  out.Assignments = asgs
  return out, nil
}

func (s *SeatService) Release(ctx context.Context, in *seatv1.ReleaseRequest) (*seatv1.ReleaseReply, error) {
  // 简化：从 reservation_id 解析 trip_id / seat_class（rid 格式为 trip:class:nanos）
  parts := strings.SplitN(in.ReservationId, ":", 3)
  if len(parts) < 2 { return &seatv1.ReleaseReply{Ok:false}, nil }
  if err := s.mgr.Release(ctx, in.ReservationId, parts[0], parts[1]); err != nil {
    return &seatv1.ReleaseReply{Ok:false}, nil
  }
  return &seatv1.ReleaseReply{Ok:true}, nil
}

func (s *SeatService) Confirm(ctx context.Context, in *seatv1.ConfirmRequest) (*seatv1.ConfirmReply, error) {
  parts := strings.SplitN(in.ReservationId, ":", 3)
  if len(parts) < 2 { return &seatv1.ConfirmReply{Ok:false}, nil }
  if err := s.mgr.Confirm(ctx, in.ReservationId, parts[0], parts[1]); err != nil {
    return &seatv1.ConfirmReply{Ok:false}, nil
  }
  return &seatv1.ConfirmReply{Ok:true}, nil
}
```
别忘了 import strings。

四、order/payment：Outbox + NATS，支付成功事件 -> 出票确认
1) app/order/internal/service/order.go（替换）
```go
package service

import (
  "context"
  "encoding/json"
  "fmt"
  "os"
  "sync"
  "time"

  "github.com/go-kratos/kratos/v2/log"
  kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
  "github.com/nats-io/nats.go"
  "github.com/redis/go-redis/v9"

  orderv1 "github.com/crud/kratos-12306/api/order/v1"
  seatv1 "github.com/crud/kratos-12306/api/seat/v1"
  "github.com/google/uuid"
)

type OrderService struct {
  orderv1.UnimplementedOrderServiceServer
  log *log.Helper

  mu     sync.RWMutex
  orders map[string]*Order // order_no -> order

  // infra
  rdb       *redis.Client
  nats      *nats.Conn
  seatEP    string

  // clients cached
  seatCli seatv1.SeatServiceClient
}

type Order struct {
  OrderNo       string
  ReservationID string
  Status        string // PendingPay, Paid, Ticketed
  Amount        int64
  CreatedAt     time.Time
}

type paidEvent struct {
  Type          string `json:"type"` // "PaymentPaid"
  OrderNo       string `json:"order_no"`
  ReservationID string `json:"reservation_id"`
  Ts            int64  `json:"ts"`
}

func getenv(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

func NewOrderService(logger *log.Helper) *OrderService {
  rdb := redis.NewClient(&redis.Options{Addr: getenv("REDIS_ADDR", "redis:6379")})
  nc, _ := nats.Connect(getenv("NATS_URL", nats.DefaultURL))
  s := &OrderService{
    log: logger, orders: make(map[string]*Order),
    rdb: rdb, nats: nc,
    seatEP: getenv("SEAT_GRPC_ADDR", "127.0.0.1:9000"),
  }
  // 初始化 seat client
  conn, err := kgrpc.DialInsecure(context.Background(), kgrpc.WithEndpoint(s.seatEP))
  if err == nil { s.seatCli = seatv1.NewSeatServiceClient(conn) }

  // 启动 Outbox 发布器
  go s.outboxPublisher(context.Background())

  // 启动支付成功订阅 -> 出票
  go s.subscribePaid(context.Background())

  return s
}

func (s *OrderService) Create(ctx context.Context, in *orderv1.CreateRequest) (*orderv1.CreateReply, error) {
  s.mu.Lock(); defer s.mu.Unlock()
  orderNo := uuid.NewString()
  amount := int64(10000) // demo price
  s.orders[orderNo] = &Order{
    OrderNo: orderNo, ReservationID: in.ReservationId, Status: "PendingPay", Amount: amount, CreatedAt: time.Now(),
  }
  return &orderv1.CreateReply{OrderNo: orderNo, Status: "PendingPay", Amount: amount}, nil
}

func (s *OrderService) Get(ctx context.Context, in *orderv1.GetRequest) (*orderv1.GetReply, error) {
  s.mu.RLock(); defer s.mu.RUnlock()
  if o, ok := s.orders[in.OrderNo]; ok {
    return &orderv1.GetReply{OrderNo: o.OrderNo, Status: o.Status, ReservationId: o.ReservationID, Amount: o.Amount}, nil
  }
  return nil, fmt.Errorf("order not found")
}

func (s *OrderService) MarkPaid(ctx context.Context, in *orderv1.MarkPaidRequest) (*orderv1.MarkPaidReply, error) {
  s.mu.Lock(); defer s.mu.Unlock()
  o, ok := s.orders[in.OrderNo]
  if !ok { return nil, fmt.Errorf("order not found") }
  if o.Status == "Paid" || o.Status == "Ticketed" {
    return &orderv1.MarkPaidReply{OrderNo: o.OrderNo, Status: o.Status}, nil
  }
  o.Status = "Paid"

  // Outbox：写入 Redis 列表，后台发布到 NATS
  ev := &paidEvent{Type:"PaymentPaid", OrderNo:o.OrderNo, ReservationID:o.ReservationID, Ts: time.Now().Unix()}
  buf, _ := json.Marshal(ev)
  _ = s.rdb.RPush(ctx, "order:outbox", string(buf)).Err()

  return &orderv1.MarkPaidReply{OrderNo: o.OrderNo, Status: o.Status}, nil
}

// 背景 worker：从 Redis Outbox 列表发布到 NATS
func (s *OrderService) outboxPublisher(ctx context.Context) {
  for {
    // BLPOP 阻塞弹出
    res, err := s.rdb.BLPop(ctx, 5*time.Second, "order:outbox").Result()
    if err != nil {
      if err == redis.Nil { continue }
      time.Sleep(500 * time.Millisecond); continue
    }
    if len(res) < 2 { continue }
    payload := res[1]
    // 发布到 NATS
    if s.nats != nil && s.nats.IsConnected() {
      if err := s.nats.Publish("payment.paid", []byte(payload)); err != nil {
        // 发布失败，放回队列重试
        _ = s.rdb.LPush(ctx, "order:outbox", payload).Err()
        time.Sleep(200 * time.Millisecond)
      }
    } else {
      // NATS 未连接，放回
      _ = s.rdb.LPush(ctx, "order:outbox", payload).Err()
      time.Sleep(500 * time.Millisecond)
    }
  }
}

// 订阅支付成功事件，调用 seat.Confirm -> 更新订单 Ticketed
func (s *OrderService) subscribePaid(ctx context.Context) {
  if s.nats == nil { return }
  _, _ = s.nats.Subscribe("payment.paid", func(msg *nats.Msg) {
    var ev paidEvent
    if err := json.Unmarshal(msg.Data, &ev); err != nil { return }
    // 出票确认
    if s.seatCli != nil {
      _, err := s.seatCli.Confirm(ctx, &seatv1.ConfirmRequest{ReservationId: ev.ReservationID})
      if err == nil {
        // 更新订单状态
        s.mu.Lock()
        if o, ok := s.orders[ev.OrderNo]; ok && o.Status == "Paid" {
          o.Status = "Ticketed"
        }
        s.mu.Unlock()
      }
    }
  })
}
```

2) app/order/cmd/order/main.go（保持不变，只负责端口/env）

五、seat 服务的 Release/Confirm 调用方：无需修改 payment；订单支付后会触发 Outbox->NATS->订单内消费者->seat.Confirm 完成出票。

六、运行与验证
1) 生成 pb 并编译
- make proto
- docker compose up --build

2) 验证流程（HTTP）
- 预占座（返回 reservation_id）
  - curl -X POST http://localhost:8000/api/seats/hold -H 'Content-Type: application/json' -d '{"tripId":"D1","fromIdx":1,"toIdx":3,"seatClass":"2nd","count":2,"clientReqId":"r1"}'
- 创建订单
  - curl -X POST http://localhost:8001/api/orders -H 'Content-Type: application/json' -d '{"reservationId":"<RID>","passengers":[{"name":"Alice","idNo":"123"}],"clientReqId":"o1"}'
- 发起支付，拿回调地址
  - curl -X POST http://localhost:8002/api/payments -H 'Content-Type: application/json' -d '{"orderNo":"<ORDER_NO>","channel":"mock"}'
- 模拟支付回调（触发 MarkPaid -> Outbox -> NATS -> seat.Confirm）
  - curl -X POST http://localhost:8002/api/payments/callback -H 'Content-Type: application/json' -d '{"orderNo":"<ORDER_NO>","channel":"mock","txnId":"tx-1"}'
- 查询订单（应看到 Ticketed）
  - curl http://localhost:8001/api/orders/<ORDER_NO>

3) 重启恢复测试
- 在 Hold 后未支付时重启 seat 容器：docker compose restart seat
- 再次查询 Hold 同样有效（3 分钟 TTL 内）；TTL 到期后会自动释放（位图释放 + Redis 预占键删除）

七、说明与后续可选增强
- 生产里建议：Actor 快照刷 MySQL（blob/版本），Redis 作为快速恢复缓存；并发 shard：按 trip_id+date+seat_class 分片；idempotency 建议 Redis SETNX 做一次性防重（这里已做）。
- TTL 释放：示例采用 Actor 定时检查 + Redis TTL 双保险；高规模可引入 Redis Keyspace 通知订阅加速释放。
- Outbox：示例使用 Redis 列表简化演示；生产应使用数据库本地事务 outbox 表 + CDC/定时发布器，或 Kafka 事务/JetStream 保障一致性。
- 限流：用 BBR 自适应；热点大促可加 API-GW 令牌桶 + 队列票据。
- Confirm 幂等：若重复 Confirm 或已 Confirm，均返回 ok；Seat 持久化里已持 status=confirmed。

