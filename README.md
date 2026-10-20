# payment-bridge — StartPay(USDT) 易支付桥接网关

独立部署的收款网关：对上游以**易支付协议**服务 new-api（new-api 保持官方原版、零改动），对下游调用 **StartPay 数字货币收单 API** 完成 USDT 收款。

```
用户浏览器 ──充值──> new-api（原版，配置为易支付平台）
                        │ submit.php（MD5 签名）
                        v
                  payment-bridge（本模块）
                        │ create_dc_order（HMAC-SHA1 签名）
                        v
                    StartPay 平台 ──> 用户在支付页完成链上付款
                        │ 异步通知（HMAC-SHA1）
                        v
                  payment-bridge ──TRADE_SUCCESS（MD5 签名）──> new-api 入账
```

## 环境变量

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `EPAY_PID` | 是 | — | 易支付商户 ID，与 new-api 的 `EpayId` 一致 |
| `EPAY_KEY` | 是 | — | 易支付商户密钥，与 new-api 的 `EpayKey` 一致 |
| `BRIDGE_PUBLIC_URL` | 是 | — | 本服务对外地址（如 `https://usdt.example.com`），用于生成 StartPay 回调地址 |
| `STARTPAY_API_KEY` | 是 | — | StartPay 商户 API Key |
| `STARTPAY_API_SECRET` | 是 | — | StartPay 商户 API Secret |
| `STARTPAY_API_BASE` | 否 | `https://api.startpay.ai/api` | StartPay API 域名前缀（测试环境换测试域名） |
| `STARTPAY_CHAIN` | 否 | `TRON` | `type=usdt` 时使用的默认链（TRON/ETH/BSC/SOL） |
| `STARTPAY_CURRENCY` | 否 | `USDT` | 收款币种（StartPay 当前仅支持 USDT） |
| `STARTPAY_CUST_FIRST_NAME` | 否 | `NewAPI` | StartPay 必填的客户名字段 |
| `STARTPAY_CUST_LAST_NAME` | 否 | `User` | StartPay 必填的客户姓氏字段 |
| `NEWAPI_NOTIFY_URL` | 否 | — | 固定回调地址。不配置时使用 new-api 下单传来的 notify_url；配置后优先使用（桥接重启丢失订单状态时也需要它兜底） |
| `BRIDGE_LISTEN` | 否 | `:8787` | 监听地址 |

## new-api 侧配置（纯界面操作，零代码改动）

新版界面位置：**系统设置 → 计费与支付 → 支付网关**。

1. 顶部先完成**支付合规确认**（root 账号），否则充值入口全部锁定；
2. **Epay** 标签页：
   - `Epay 接口地址`（PayAddress）= 桥接网关地址，如 `https://api.example.com/bridge`
   - `Epay 商户 ID`（EpayId）= `EPAY_PID` 的值
   - `Epay 密钥`（EpayKey）= `EPAY_KEY` 的值
3. **通用** 标签页：
   - `价格（本地货币 / USD）`（Price）= **每 1 美元余额收取的 USDT 数量**（默认 1；要覆盖手续费/溢价可设 1.02~1.05）
   - 支付方式（PayMethods）加一条：`{"name":"USDT-TRC20","type":"usdt-tron"}`（可视化编辑器直接填，或 JSON）
4. 用户充值页即出现 USDT 支付方式，点击后跳转 StartPay 收银台。

> Epay 标签里的「回调地址」（CustomCallbackAddress）保持留空——本方案回调地址由桥接的 `BRIDGE_PUBLIC_URL` 决定，不要覆盖。

## 支付类型与链的映射

`type` 参数决定 StartPay 的区块链网络：

| type | 链 |
|---|---|
| `usdt` | `STARTPAY_CHAIN` 指定的默认链 |
| `usdt-tron` / `usdt-trx` / `usdt-trc20` | TRON |
| `usdt-eth` / `usdt-erc20` | ETH |
| `usdt-bsc` / `usdt-bep20` | BSC |
| `usdt-sol` / `usdt-solana` | SOL |

其他类型（如 `alipay`）会被拒绝——本网关只做 USDT 收款，混合支付请部署多个网关实例。

## 入账规则（与安全）

- StartPay 通知先验签（HMAC-SHA1 + 时间戳 30 分钟防重放），再校验**实际到账 ≥ 订单金额**才转发 `TRADE_SUCCESS`；少付（`less`）不转发，桥接日志中告警，需人工处理；
- 重复通知幂等（同一订单只回调一次 new-api；new-api 侧另有行锁事务双重防重）；
- 回调 new-api 失败自动重试 3 次，仍失败返回 500 让 StartPay 稍后重推；
- 桥接重启丢失订单状态时，通过 StartPay 查单重建回调参数（需配置 `NEWAPI_NOTIFY_URL`）；
- 双向密钥分离：`EPAY_KEY` 只在 new-api 与桥接之间使用，`STARTPAY_API_SECRET` 只在桥接与 StartPay 之间使用。

## 部署

docker-compose 示例：

```yaml
services:
  payment-bridge:
    build: ./payment-bridge
    restart: unless-stopped
    environment:
      EPAY_PID: "1001"
      EPAY_KEY: "change-me-to-a-long-random-secret"
      BRIDGE_PUBLIC_URL: "https://usdt.example.com"
      STARTPAY_API_KEY: "your-startpay-key"
      STARTPAY_API_SECRET: "your-startpay-secret"
      NEWAPI_NOTIFY_URL: "https://newapi.example.com/api/user/epay/notify"
    ports:
      - "8787:8787"
```

`BRIDGE_PUBLIC_URL` 需反代到本服务并配置 HTTPS（StartPay 回调与用户跳转都走它）。

## 路径前缀与负载均衡

桥接按 `BRIDGE_PUBLIC_URL` 的路径部分自动注册路由：配置 `https://api.example.com/bridge` 就在 `/bridge/submit.php`、`/bridge/notify/startpay` 下提供服务，因此反代/负载均衡**无需路径重写**（ALB 也不支持重写）。`/healthz` 始终同时挂在根路径与前缀路径下，便于 LB 直连健康检查。

阿里云 ALB/CLB 典型接法：

1. 服务器组：新建一个只含桥接 ECS 的组（`内网IP:8787`），健康检查路径 `/healthz`
2. 443 监听器加转发规则：URL 路径 `/bridge/*` → 桥接服务器组；默认规则保持 new-api 不变
3. 安全组：放行入方向 TCP 8787 给 ALB 回源网段 `100.64.0.0/10`；不对公网开放 8787
4. `BRIDGE_LISTEN` 必须 `0.0.0.0:8787`——ALB 从 VM 外部连后端，绑 `127.0.0.1` 连不上（同机 nginx 反代场景才绑回环）

> 排查 404：`404 page not found` 是 Go 服务默认 404。先在 ECS 上 `curl http://127.0.0.1:8787/healthz` 确认存活；再 `curl http://127.0.0.1:8787/bridge/healthz`——前者 ok、后者 404 说明桥接镜像过旧（不认前缀，重新构建）；本机都 ok 但走 ALB 仍 404，说明转发规则未命中（检查规则路径条件与优先级，请求可能被默认规则送到了 new-api）。

## 本地开发

```bash
cd payment-bridge
GOWORK=off go build ./...   # 独立模块，必须脱离 workspace 可构建
GOWORK=off go test ./...
```
