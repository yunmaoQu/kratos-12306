好的，我们把支付服务从“模拟”升级为对接微信支付、支付宝、银联（全渠道）的可落地方案。思路是：在现有 payment-service 中引入多通道能力，统一下单接口，按 channel/scene 走不同网关，签名与回调验签后回调 order.MarkPaid，后续照你的 Outbox+NATS 流水完成“出票确认”。

目录
- 方案总览与改动点
- API 设计（统一创建、分通道回调）
- 配置与证书管理（env/文件/挂载）
- 实现要点（Channel 接口 + 三通道落地）
  - 微信支付 v3
  - 支付宝
  - 银联全渠道
- 回调与幂等、事件流
- Docker Compose 及运行步骤
- 合规与生产注意事项

一、方案总览与改动点
- 支付服务聚合三通道：
  - 微信支付 v3（Native/H5/JSAPI）
  - 支付宝（PC/WAP/APP）
  - 银联全渠道（前台/后台跳转）
- 统一下单：POST /api/payments
  - 入参指定 channel/wechatScene 等，返回前端不同形态参数（二维码链接、mweb_url、JSAPI 参数、支付宝 orderStr、银联自动提交表单 HTML）
- 分通道回调：
  - /api/payments/callback/wechat（JSON + 签名头）
  - /api/payments/callback/alipay（form-urlencoded）
  - /api/payments/callback/unionpay（form-urlencoded）
- 回调处理：
  - 验签成功 -> 调 order.MarkPaid(order_no, channel, txn_id) -> 由 order 服务写 Outbox 并发布支付成功事件 -> seat.Confirm 出票
- 幂等：
  - 支付回调使用 txn_id 唯一（支付网关交易号）+去重表/缓存，重复回调直接 200
- 金额统一用分（int64），避免浮点

二、API 设计（proto 修改）
api/payment/v1/payment.proto
```proto
syntax = "proto3";
package payment.v1;
option go_package = "api/payment/v1;v1";
import "google/api/annotations.proto";

service PaymentService {
  rpc Create(CreatePaymentRequest) returns (CreatePaymentReply) {
    option (google.api.http) = { post:"/api/payments", body:"*" };
  }
  // 回调（分别为三通道）
  rpc WechatNotify(WechatNotifyRequest) returns (NotifyReply) {
    option (google.api.http) = { post:"/api/payments/callback/wechat", body:"*" };
  }
  rpc AlipayNotify(AlipayNotifyRequest) returns (NotifyReply) {
    option (google.api.http) = { post:"/api/payments/callback/alipay", body:"*" };
  }
  rpc UnionPayNotify(UnionPayNotifyRequest) returns (NotifyReply) {
    option (google.api.http) = { post:"/api/payments/callback/unionpay", body:"*" };
  }
}

// 统一下单
message CreatePaymentRequest {
  string order_no = 1;
  string channel = 2;  // wechat | alipay | unionpay
  string scene = 3;    // wechat: jsapi|native|h5 ; alipay: page|wap|app ; unionpay: front|app
  int64  amount = 4;   // 分
  string subject = 5;
  // scene 扩展参数
  string wechat_openid = 10;  // JSAPI 需要
  string client_ip = 11;      // H5/银联可能需要
  string return_url = 12;     // 支付后同步返回页（支付宝/银联前台）
}
message CreatePaymentReply {
  string pay_url = 1;         // H5 URL 或二维码跳转 URL（视场景）
  string qr_code_url = 2;     // 微信 Native 的 code_url
  string mweb_url = 3;        // 微信 H5 mweb_url
  string alipay_order_str = 4;// 支付宝 APP 的 orderStr
  string html = 5;            // 银联前台自动提交表单（可直接渲染）
  map<string,string> extra = 10; // 微信 JSAPI 参数等
}

// 回调请求体用原始 HTTP 请求读取，此处定义空壳以适配 kratos http 入口
message WechatNotifyRequest {}
message AlipayNotifyRequest {}
message UnionPayNotifyRequest {}
message NotifyReply { bool ok = 1; }
```
执行 make proto 生成。

三、配置与证书管理
- 建议用环境变量 + 文件挂载（Docker secrets/volume）
- 关键配置（示例 env 名称）：
  - 通用
    - PAY_NOTIFY_BASE=https://your.domain.com  用于拼接回调 notify_url
  - WeChat Pay v3
    - WECHAT_MCH_ID、WECHAT_APP_ID、WECHAT_MCH_CERT_SERIAL
    - WECHAT_API_V3_KEY
    - WECHAT_MCH_PRIVATE_KEY=/secrets/wechat/apiclient_key.pem
    - 可选：平台证书自动下载，使用 wechatpay-go AutoAuthCipher
  - Alipay
    - ALIPAY_APP_ID
    - ALIPAY_IS_PROD=true|false
    - ALIPAY_PRIVATE_KEY=/secrets/alipay/app_private_key.pem
    - ALIPAY_PUBLIC_KEY=/secrets/alipay/alipay_public_key.pem
  - UnionPay（全渠道）
    - UP_MER_ID
    - UP_SIGN_CERT=/secrets/unionpay/mer_sign.pfx
    - UP_SIGN_CERT_PWD=xxxxxx
    - UP_ENCRYPT_CERT=/secrets/unionpay/encrypt.cer
    - UP_MIDDLE_CERT=/secrets/unionpay/middle.cer
    - UP_ROOT_CERT=/secrets/unionpay/root.cer
    - UP_GATEWAY=https://gateway.95516.com/gateway/api/frontTransReq.do
- docker-compose 里将 secrets/* 挂载进容器

四、实现要点（Channel 接口 + 三通道）
1) 定义通道接口与工厂
app/payment/internal/channel/channel.go
```go
package channel

import (
  "context"
  "net/http"
)

type CreateParams struct {
  OrderNo string
  Amount  int64 // 分
  Subject string
  Scene   string
  OpenID  string
  ClientIP string
  ReturnURL string
  NotifyURL string
}

type CreateResult struct {
  PayURL      string
  QRCodeURL   string
  MwebURL     string
  AlipayOrderStr string
  HTML        string
  Extra       map[string]string // JSAPI params 等
}

type NotifyResult struct {
  OK       bool
  OrderNo  string
  TxnID    string
  Amount   int64
  Raw      []byte
}

type Channel interface {
  Create(ctx context.Context, p *CreateParams) (*CreateResult, error)
  HandleNotify(ctx context.Context, r *http.Request) (*NotifyResult, error)
}
```

2) 微信支付 v3
依赖
- github.com/wechatpay-apiv3/wechatpay-go

app/payment/internal/channel/wechat.go
```go
package channel

import (
  "context"
  "crypto/rsa"
  "encoding/json"
  "errors"
  "io"
  "net/http"
  "time"

  "github.com/wechatpay-apiv3/wechatpay-go/core"
  "github.com/wechatpay-apiv3/wechatpay-go/core/option"
  "github.com/wechatpay-apiv3/wechatpay-go/core/notify"
  "github.com/wechatpay-apiv3/wechatpay-go/services/payments/h5"
  "github.com/wechatpay-apiv3/wechatpay-go/services/payments/jsapi"
  "github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
  "github.com/wechatpay-apiv3/wechatpay-go/utils"
)

type WechatConfig struct {
  MchID, AppID, MchCertSN, APIv3Key string
  PrivateKey *rsa.PrivateKey
  NotifyBase string // PAY_NOTIFY_BASE
}

type WechatChannel struct {
  cfg WechatConfig
  cli *core.Client
  verifier core.Verifier
}

func NewWechat(cfg WechatConfig) (*WechatChannel, error) {
  opts := []core.ClientOption{
    option.WithMerchant(cfg.MchID, cfg.MchCertSN, cfg.PrivateKey),
    option.WithWechatPayAutoAuthCipher(cfg.APIv3Key),
  }
  cli, err := core.NewClient(context.Background(), opts...)
  if err != nil { return nil, err }
  v, err := core.NewCertificateVerifier(cli)
  if err != nil { return nil, err }
  return &WechatChannel{cfg: cfg, cli: cli, verifier: v}, nil
}

func (w *WechatChannel) Create(ctx context.Context, p *CreateParams) (*CreateResult, error) {
  notifyURL := w.cfg.NotifyBase + "/api/payments/callback/wechat"
  switch p.Scene {
  case "native":
    svc := native.NativeApiService{Client: w.cli}
    req := native.PrepayRequest{
      Appid: core.String(w.cfg.AppID), Mchid: core.String(w.cfg.MchID),
      Description: core.String(p.Subject), OutTradeNo: core.String(p.OrderNo),
      NotifyUrl: core.String(notifyURL),
      Amount: &native.Amount{Total: core.Int64(p.Amount)},
      TimeExpire: core.Time(time.Now().Add(15*time.Minute)),
    }
    resp, _, err := svc.Prepay(ctx, req)
    if err != nil { return nil, err }
    return &CreateResult{QRCodeURL: *resp.CodeUrl}, nil

  case "h5":
    svc := h5.H5ApiService{Client: w.cli}
    req := h5.PrepayRequest{
      Appid: core.String(w.cfg.AppID), Mchid: core.String(w.cfg.MchID),
      Description: core.String(p.Subject), OutTradeNo: core.String(p.OrderNo),
      NotifyUrl: core.String(notifyURL),
      Amount: &h5.Amount{Total: core.Int64(p.Amount)},
      SceneInfo: &h5.SceneInfo{PayerClientIp: core.String(p.ClientIP)},
    }
    resp, _, err := svc.Prepay(ctx, req)
    if err != nil { return nil, err }
    return &CreateResult{MwebURL: *resp.H5Url}, nil

  case "jsapi":
    if p.OpenID == "" { return nil, errors.New("wechat jsapi requires openid") }
    svc := jsapi.JsapiApiService{Client: w.cli}
    req := jsapi.PrepayRequest{
      Appid: core.String(w.cfg.AppID), Mchid: core.String(w.cfg.MchID),
      Description: core.String(p.Subject), OutTradeNo: core.String(p.OrderNo),
      NotifyUrl: core.String(notifyURL),
      Amount: &jsapi.Amount{Total: core.Int64(p.Amount)},
      Payer: &jsapi.Payer{Openid: core.String(p.OpenID)},
    }
    resp, _, err := svc.Prepay(ctx, req)
    if err != nil { return nil, err }

    // 生成前端调起参数
    ts := time.Now().Unix()
    nonce, _ := utils.GenerateNonce()
    pkg := "prepay_id=" + *resp.PrepayId
    sign, err := utils.SignSHA256WithRSA(w.cfg.PrivateKey, []byte(w.cfg.AppID+"\n"+core.StringToString(ts)+"\n"+nonce+"\n"+pkg+"\n"))
    if err != nil { return nil, err }
    extra := map[string]string{
      "appId": w.cfg.AppID,
      "timeStamp": core.StringToString(ts),
      "nonceStr": nonce,
      "package": pkg,
      "signType": "RSA",
      "paySign": sign,
    }
    return &CreateResult{Extra: extra}, nil
  default:
    return nil, errors.New("unsupported wechat scene")
  }
}

func (w *WechatChannel) HandleNotify(ctx context.Context, r *http.Request) (*NotifyResult, error) {
  body, _ := io.ReadAll(r.Body)
  handler := notify.NewNotifyHandler(w.cfg.APIv3Key, w.verifier)
  event, err := handler.ParseNotifyRequest(ctx, r, body)
  if err != nil { return nil, err }
  // 解密资源
  b, err := event.Resource.DecryptCipherText(w.cfg.APIv3Key)
  if err != nil { return nil, err }
  var txn struct {
    OutTradeNo string `json:"out_trade_no"`
    TransactionId string `json:"transaction_id"`
    TradeState string `json:"trade_state"`
    Amount struct{ PayerTotal int64 `json:"payer_total"` } `json:"amount"`
  }
  if err := json.Unmarshal(b, &txn); err != nil { return nil, err }
  if txn.TradeState != "SUCCESS" {
    return &NotifyResult{OK: false, OrderNo: txn.OutTradeNo, TxnID: txn.TransactionId, Raw: body}, nil
  }
  return &NotifyResult{
    OK: true, OrderNo: txn.OutTradeNo, TxnID: txn.TransactionId, Amount: txn.Amount.PayerTotal, Raw: body,
  }, nil
}
```
要点
- v3：商户私钥签名、平台证书自动校验
- 回调验签：用 notify.Handler + 平台证书 verifier
- JSAPI 需 openid

3) 支付宝
依赖
- github.com/smartwalle/alipay/v3

app/payment/internal/channel/alipay.go
```go
package channel

import (
  "context"
  "net/http"
  "net/url"

  "github.com/smartwalle/alipay/v3"
)

type AlipayConfig struct {
  AppID string
  IsProd bool
  PrivateKey string   // 内容字符串（或从文件读取）
  AlipayPublicKey string
  NotifyBase string
}

type AlipayChannel struct {
  cfg AlipayConfig
  cli *alipay.Client
}

func NewAlipay(cfg AlipayConfig) (*AlipayChannel, error) {
  c, err := alipay.New(cfg.AppID, cfg.PrivateKey, cfg.IsProd)
  if err != nil { return nil, err }
  if err := c.LoadAliPayPublicKey(cfg.AlipayPublicKey); err != nil { return nil, err }
  return &AlipayChannel{cfg: cfg, cli: c}, nil
}

func (a *AlipayChannel) Create(ctx context.Context, p *CreateParams) (*CreateResult, error) {
  notifyURL := a.cfg.NotifyBase + "/api/payments/callback/alipay"
  returnURL := p.ReturnURL
  switch p.Scene {
  case "page": // 电脑网站
    var param = alipay.TradePagePay{}
    param.NotifyURL = notifyURL
    param.ReturnURL = returnURL
    param.Subject = p.Subject
    param.OutTradeNo = p.OrderNo
    param.TotalAmount = formatYuan(p.Amount) // 分->元字符串
    param.ProductCode = "FAST_INSTANT_TRADE_PAY"
    u, err := a.cli.TradePagePay(param)
    if err != nil { return nil, err }
    return &CreateResult{PayURL: u.String()}, nil

  case "wap": // 手机网站
    var param = alipay.TradeWapPay{}
    param.NotifyURL = notifyURL
    param.ReturnURL = returnURL
    param.Subject = p.Subject
    param.OutTradeNo = p.OrderNo
    param.TotalAmount = formatYuan(p.Amount)
    param.ProductCode = "QUICK_WAP_WAY"
    u, err := a.cli.TradeWapPay(param)
    if err != nil { return nil, err }
    return &CreateResult{PayURL: u.String()}, nil

  case "app": // App
    var param = alipay.TradeAppPay{}
    param.NotifyURL = notifyURL
    param.Subject = p.Subject
    param.OutTradeNo = p.OrderNo
    param.TotalAmount = formatYuan(p.Amount)
    s, err := a.cli.TradeAppPay(param)
    if err != nil { return nil, err }
    return &CreateResult{AlipayOrderStr: s}, nil
  default:
    return nil, ErrUnsupportedScene
  }
}

func (a *AlipayChannel) HandleNotify(ctx context.Context, r *http.Request) (*NotifyResult, error) {
  if err := r.ParseForm(); err != nil { return nil, err }
  ok, err := a.cli.VerifySign(r.Form)
  if err != nil || !ok { return &NotifyResult{OK:false}, err }
  orderNo := r.Form.Get("out_trade_no")
  tradeNo := r.Form.Get("trade_no")
  tradeStatus := r.Form.Get("trade_status")
  amountYuan := r.Form.Get("total_amount")
  amountFen := parseFen(amountYuan)
  if tradeStatus == "TRADE_SUCCESS" || tradeStatus == "TRADE_FINISHED" {
    return &NotifyResult{OK:true, OrderNo: orderNo, TxnID: tradeNo, Amount: amountFen}, nil
  }
  return &NotifyResult{OK:false, OrderNo: orderNo, TxnID: tradeNo}, nil
}

func formatYuan(fen int64) string {
  // 分 -> 元字符串，保留2位
  y := fen / 100; c := fen % 100
  return url.QueryEscape(fmt.Sprintf("%d.%02d", y, c))
}
```
说明
- page/wap 返回跳转 URL；app 返回 orderStr 交给 SDK
- 回调验签：VerifySign + trade_status 判断

4) 银联全渠道（Front Trans）
说明
- 以“前台跳转”方式示例（最易集成），返回一段 HTML 表单，前端直接渲染自动提交
- 参数签名使用商户证书（pfx）/私钥，验签使用银联根/中间证书
- 你可以使用现成 SDK（示例：github.com/rongl/unionpaysdk），也可自行实现签名

示例（简化，自签名流程略）
app/payment/internal/channel/unionpay.go
```go
package channel

import (
  "context"
  "crypto"
  "crypto/x509"
  "encoding/pem"
  "errors"
  "fmt"
  "net/http"
  "net/url"
  "strings"
  "time"
)

type UnionPayConfig struct {
  MerID string
  Gateway string // https://gateway.95516.com/gateway/api/frontTransReq.do
  SignCertPath string
  SignCertPwd  string
  EncryptCertPath string
  MiddleCertPath string
  RootCertPath   string
  NotifyBase string
}

type UnionPayChannel struct {
  cfg UnionPayConfig
  // 缓存私钥/证书等（略）
}

func NewUnionPay(cfg UnionPayConfig) (*UnionPayChannel, error) {
  return &UnionPayChannel{cfg: cfg}, nil
}

func (u *UnionPayChannel) Create(ctx context.Context, p *CreateParams) (*CreateResult, error) {
  if p.Scene != "front" {
    return nil, errors.New("only front scene demo implemented")
  }
  params := map[string]string{
    "version":"5.1.0", "encoding":"utf-8", "signMethod":"01", "txnType":"01", "txnSubType":"01",
    "bizType":"000201", "channelType":"08",
    "merId": u.cfg.MerID, "accessType":"0",
    "orderId": p.OrderNo,
    "txnTime": time.Now().Format("20060102150405"),
    "txnAmt": fmt.Sprintf("%d", p.Amount),
    "currencyCode":"156",
    "frontUrl": p.ReturnURL,
    "backUrl": u.cfg.NotifyBase + "/api/payments/callback/unionpay",
  }
  // 1) 参数签名（加载 pfx，生成签名，附加 certId/sign）——省略详细实现
  signed := SignUnionParams(params, u.cfg.SignCertPath, u.cfg.SignCertPwd) // 需自行实现或用 SDK

  // 2) 输出自动提交表单 HTML
  var b strings.Builder
  b.WriteString(`<html><body onload="document.forms[0].submit()">`)
  b.WriteString(`<form id="pay" method="post" action="` + u.cfg.Gateway + `">`)
  for k, v := range signed {
    b.WriteString(fmt.Sprintf(`<input type="hidden" name="%s" value='%s'/>`, k, htmlEscape(v)))
  }
  b.WriteString(`</form></body></html>`)
  return &CreateResult{HTML: b.String()}, nil
}

func (u *UnionPayChannel) HandleNotify(ctx context.Context, r *http.Request) (*NotifyResult, error) {
  if err := r.ParseForm(); err != nil { return nil, err }
  m := map[string]string{}
  for k := range r.PostForm { m[k] = r.PostForm.Get(k) }
  // 1) 验签（使用银联根/中间证书链）——省略详细实现，或用 SDK
  if !VerifyUnionSign(m, u.cfg.MiddleCertPath, u.cfg.RootCertPath) {
    return &NotifyResult{OK:false}, errors.New("verify failed")
  }
  if m["respCode"] == "00" {
    orderNo := m["orderId"]; txnId := m["queryId"]
    amt := parseInt64(m["txnAmt"])
    return &NotifyResult{OK:true, OrderNo: orderNo, TxnID: txnId, Amount: amt}, nil
  }
  return &NotifyResult{OK:false}, nil
}
```
备注
- 银联签名/验签实现较多细节，建议先用社区 SDK 打通（或官方 Java/PHP 文档对照 Go 实现）
- 生产务必完成证书自动更新与 CRL 校验

5) PaymentService 聚合路由
app/payment/internal/service/payment.go（替换）
```go
package service

import (
  "context"
  "fmt"
  "net/http"
  "os"

  "github.com/go-kratos/kratos/v2/log"
  khttp "github.com/go-kratos/kratos/v2/transport/http"
  kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"

  orderv1 "github.com/crud/kratos-12306/api/order/v1"
  payv1 "github.com/crud/kratos-12306/api/payment/v1"
  "github.com/crud/kratos-12306/app/payment/internal/channel"

  "github.com/wechatpay-apiv3/wechatpay-go/utils"
)

type PaymentService struct {
  payv1.UnimplementedPaymentServiceServer
  log *log.Helper

  orderEP string
  orderCli orderv1.OrderServiceClient

  // channels
  wechat  channel.Channel
  alipay  channel.Channel
  union   channel.Channel
}

func getenv(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

func NewPaymentService(logger *log.Helper) *PaymentService {
  s := &PaymentService{log: logger}
  // order client
  ep := getenv("ORDER_GRPC_ADDR", "order:9001")
  s.orderEP = ep
  conn, err := kgrpc.DialInsecure(context.Background(), kgrpc.WithEndpoint(ep))
  if err == nil { s.orderCli = orderv1.NewOrderServiceClient(conn) }

  // init channels
  notifyBase := getenv("PAY_NOTIFY_BASE", "http://localhost:8002")

  // Wechat
  if mchID := os.Getenv("WECHAT_MCH_ID"); mchID != "" {
    skPath := getenv("WECHAT_MCH_PRIVATE_KEY", "/secrets/wechat/apiclient_key.pem")
    pk, _ := utils.LoadPrivateKeyWithPath(skPath)
    w, err := channel.NewWechat(channel.WechatConfig{
      MchID: mchID, AppID: os.Getenv("WECHAT_APP_ID"),
      MchCertSN: os.Getenv("WECHAT_MCH_CERT_SERIAL"),
      APIv3Key: os.Getenv("WECHAT_API_V3_KEY"),
      PrivateKey: pk, NotifyBase: notifyBase,
    })
    if err == nil { s.wechat = w }
  }
  // Alipay
  if appID := os.Getenv("ALIPAY_APP_ID"); appID != "" {
    pri := mustReadAll(getenv("ALIPAY_PRIVATE_KEY","/secrets/alipay/app_private_key.pem"))
    pub := mustReadAll(getenv("ALIPAY_PUBLIC_KEY","/secrets/alipay/alipay_public_key.pem"))
    a, err := channel.NewAlipay(channel.AlipayConfig{
      AppID: appID, IsProd: getenv("ALIPAY_IS_PROD","false")=="true",
      PrivateKey: string(pri), AlipayPublicKey: string(pub), NotifyBase: notifyBase,
    })
    if err == nil { s.alipay = a }
  }
  // UnionPay
  if mer := os.Getenv("UP_MER_ID"); mer != "" {
    u, err := channel.NewUnionPay(channel.UnionPayConfig{
      MerID: mer, Gateway: getenv("UP_GATEWAY","https://gateway.95516.com/gateway/api/frontTransReq.do"),
      SignCertPath: os.Getenv("UP_SIGN_CERT"), SignCertPwd: os.Getenv("UP_SIGN_CERT_PWD"),
      EncryptCertPath: os.Getenv("UP_ENCRYPT_CERT"),
      MiddleCertPath: os.Getenv("UP_MIDDLE_CERT"), RootCertPath: os.Getenv("UP_ROOT_CERT"),
      NotifyBase: notifyBase,
    })
    if err == nil { s.union = u }
  }
  return s
}

func (s *PaymentService) Create(ctx context.Context, in *payv1.CreatePaymentRequest) (*payv1.CreatePaymentReply, error) {
  p := &channel.CreateParams{
    OrderNo: in.OrderNo, Amount: in.Amount, Subject: in.Subject,
    Scene: in.Scene, OpenID: in.WechatOpenid, ClientIP: in.ClientIp, ReturnURL: in.ReturnUrl,
    NotifyURL: getenv("PAY_NOTIFY_BASE","") + "/api/payments/callback/" + in.Channel,
  }
  var res *channel.CreateResult
  var err error
  switch in.Channel {
  case "wechat":
    if s.wechat == nil { return nil, fmt.Errorf("wechat not configured") }
    res, err = s.wechat.Create(ctx, p)
  case "alipay":
    if s.alipay == nil { return nil, fmt.Errorf("alipay not configured") }
    res, err = s.alipay.Create(ctx, p)
  case "unionpay":
    if s.union == nil { return nil, fmt.Errorf("unionpay not configured") }
    res, err = s.union.Create(ctx, p)
  default:
    return nil, fmt.Errorf("unsupported channel")
  }
  if err != nil { return nil, err }
  return &payv1.CreatePaymentReply{
    PayUrl: res.PayURL, QrCodeUrl: res.QRCodeURL, MwebUrl: res.MwebURL,
    AlipayOrderStr: res.AlipayOrderStr, Html: res.HTML, Extra: res.Extra,
  }, nil
}

func (s *PaymentService) WechatNotify(ctx context.Context, _ *payv1.WechatNotifyRequest) (*payv1.NotifyReply, error) {
  if s.wechat == nil { return &payv1.NotifyReply{Ok:true}, nil }
  req, _ := khttp.RequestFromServerContext(ctx)
  n, err := s.wechat.HandleNotify(ctx, req)
  if err != nil || !n.OK { return &payv1.NotifyReply{Ok:false}, nil }
  // 标记已支付
  _, _ = s.orderCli.MarkPaid(ctx, &orderv1.MarkPaidRequest{OrderNo: n.OrderNo, Channel: "wechat", TxnId: n.TxnID})
  return &payv1.NotifyReply{Ok:true}, nil
}

func (s *PaymentService) AlipayNotify(ctx context.Context, _ *payv1.AlipayNotifyRequest) (*payv1.NotifyReply, error) {
  if s.alipay == nil { return &payv1.NotifyReply{Ok:true}, nil }
  req, _ := khttp.RequestFromServerContext(ctx)
  n, err := s.alipay.HandleNotify(ctx, req)
  if err != nil || !n.OK { return &payv1.NotifyReply{Ok:false}, nil }
  _, _ = s.orderCli.MarkPaid(ctx, &orderv1.MarkPaidRequest{OrderNo: n.OrderNo, Channel: "alipay", TxnId: n.TxnID})
  return &payv1.NotifyReply{Ok:true}, nil
}

func (s *PaymentService) UnionPayNotify(ctx context.Context, _ *payv1.UnionPayNotifyRequest) (*payv1.NotifyReply, error) {
  if s.union == nil { return &payv1.NotifyReply{Ok:true}, nil }
  req, _ := khttp.RequestFromServerContext(ctx)
  n, err := s.union.HandleNotify(ctx, req)
  if err != nil || !n.OK { return &payv1.NotifyReply{Ok:false}, nil }
  _, _ = s.orderCli.MarkPaid(ctx, &orderv1.MarkPaidRequest{OrderNo: n.OrderNo, Channel: "unionpay", TxnId: n.TxnID})
  return &payv1.NotifyReply{Ok:true}, nil
}

func mustReadAll(path string) []byte { b, _ := os.ReadFile(path); return b }
```
注意
- 我们保留“回调 -> 调 order.MarkPaid -> Outbox/NATS 出票”的链路
- HTML（银联前台）直接返回给前端渲染或服务端中转

五、回调幂等与事件流
- 幂等：建议在 order.MarkPaid 内以 txn_id 唯一索引去重（已在你前文设计里）
- 失败重试：支付回调失败时，渠道会多次调用；我们处理成功后应返回 200/OK 或渠道要求的格式（微信可返回空 200，支付宝返回 success 字符串，银联返回 http 200）
  - Kratos HTTP 默认返回 200/JSON；如需 exact body，可在回调方法里直接写 ResponseWriter（可扩展）
- 事件流：订单服务的 MarkPaid 已写 Outbox -> NATS -> seat.Confirm，保持不变

六、Docker Compose 与运行
docker-compose.yml（新增 secrets 挂载与环境变量）
```yaml
services:
  payment:
    build: ./app/payment
    image: crud/payment:dev
    environment:
      - ORDER_GRPC_ADDR=order:9001
      - PAY_NOTIFY_BASE=http://payment:8002
      # WeChat
      - WECHAT_MCH_ID=YOUR_MCH_ID
      - WECHAT_APP_ID=YOUR_APP_ID
      - WECHAT_MCH_CERT_SERIAL=YOUR_CERT_SN
      - WECHAT_API_V3_KEY=YOUR_V3_KEY
      - WECHAT_MCH_PRIVATE_KEY=/secrets/wechat/apiclient_key.pem
      # Alipay
      - ALIPAY_APP_ID=YOUR_APP_ID
      - ALIPAY_IS_PROD=false
      - ALIPAY_PRIVATE_KEY=/secrets/alipay/app_private_key.pem
      - ALIPAY_PUBLIC_KEY=/secrets/alipay/alipay_public_key.pem
      # UnionPay
      - UP_MER_ID=YOUR_MER_ID
      - UP_SIGN_CERT=/secrets/unionpay/mer_sign.pfx
      - UP_SIGN_CERT_PWD=000000
      - UP_ENCRYPT_CERT=/secrets/unionpay/encrypt.cer
      - UP_MIDDLE_CERT=/secrets/unionpay/middle.cer
      - UP_ROOT_CERT=/secrets/unionpay/root.cer
      - UP_GATEWAY=https://gateway.95516.com/gateway/api/frontTransReq.do
    volumes:
      - ./secrets:/secrets:ro
    ports: ["8002:8002","9002:9002"]
```

七、前后端联调说明
- 微信
  - Native：后端返回 qr_code_url，前端生成二维码
  - H5：mweb_url 需在微信外浏览器打开，或按官方指引在特定场景跳转
  - JSAPI：前端拿 extra 中的参数直接调起 wx.requestPayment
- 支付宝
  - page/wap：后端返回 url，前端 window.location = url
  - app：返回 orderStr，移动端 SDK 调起
- 银联
  - 前台 front：后端返回 html，前端 innerHTML 渲染或新窗口打开

八、合规与生产注意
- HTTPS 与备案：支付回调与前端域名必须 HTTPS 且已备案
- 证书轮换与安全：商户私钥仅存于服务端（KMS/密钥管理），定期轮换平台证书（微信/银联）
- 金额与重复支付：金额严格以分校验；订单状态机防重复 MarkPaid
- 监控告警：通道错误率、回调验签失败、对账差异
- 日志脱敏：仅记录必要字段，屏蔽卡号/姓名等敏感信息

需要我把上述代码按你的仓库结构生成具体文件，或给一份可直接 clone 的分支吗？你也可以告诉我你优先支持的场景（微信 JSAPI/Native、支付宝 WAP/APP、银联 Front/App），我就按该优先级把模板补齐到可编译通过的完整版本。