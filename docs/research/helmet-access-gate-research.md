# HTTP “Helmet” 访问门禁方案研究

研究日期：2026-08-29

## 结论

可以实现，而且不需要改动 4001 端口上的原服务：让它只监听 `127.0.0.1:4001`（或只存在于容器内部网络），公网只暴露一个 HTTPS 反向代理。代理对明确配置的公开 URL 直接放行；其余 URL 只有在服务端验证会话后才转发到 4001，否则返回真正的 HTTP 404 和仿 NGINX 的极简页面。

但需求中的两个判断不能作为安全边界：

- **不能通过 User-Agent 判断“是不是浏览器”并让非浏览器直接通过。** `User-Agent` 只是客户端发送的普通请求头；RFC 9110 只规定了它的格式和用途，没有赋予它可信身份。`curl`、扫描器和攻击脚本都能发送任意浏览器 User-Agent，也能省略它。参见 [RFC 9110 §10.1.5](https://www.rfc-editor.org/rfc/rfc9110.html#name-user-agent)。
- **不能把 `localStorage.unlocked=true` 当作授权。** Web Storage 是同源页面脚本可读写的客户端状态，不会像 Cookie 一样自动随 HTTP 请求发送；用户也可以在开发者工具中任意修改。WHATWG 将它定义为页面所属 origin 的本地存储区；OWASP 明确建议不要把会话标识或凭据放入 Web Storage。参见 [WHATWG Web Storage](https://html.spec.whatwg.org/multipage/webstorage.html) 和 [OWASP Session Management：HTML5 Web Storage](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html#html5-web-storage-api)。

因此，“点击无害文字 20 次”最多是一个**降低误发现和扫描噪声的机关**，不是认证。任何人都能查看前端脚本、发现解锁接口并直接调用它。如果目标是抵抗真实攻击，20 次点击应该只负责显示真正的认证入口，随后必须验证密码、一次性口令、Passkey/WebAuthn、OIDC，或者其他服务端凭据。

## 推荐架构

```text
Internet
   |
   v
HTTPS :443
NGINX / Caddy
   |-- 精确的公开路径 allowlist ------------> 127.0.0.1:4001
   |-- /_helmet/unlock ---------------------> 127.0.0.1:4002 门禁服务
   |-- 受保护路径 -- 服务端验证会话 Cookie --+--> 127.0.0.1:4001
   |                                         |
   +-- 未授权 <------------------------------+-- 返回真实 404 + 伪装页

公网或局域网不能直接连接 :4001
```

安全性最重要的一条是：**4001 必须无法绕过代理直连**。只在代理前面返回 404、但仍把 4001 暴露在公网，没有保护作用。

### 建议的请求流程

1. 首次访问受保护 URL 时，代理向门禁服务发内部鉴权请求。
2. 没有有效会话时返回 `404`，响应体是极简仿 NGINX 页面，并带 `Cache-Control: no-store`。不要返回 `WWW-Authenticate`、认证重定向或包含后台名称的错误信息。
3. 页面脚本在同一元素累计 20 次点击。达到次数后：
   - 仅需防止普通扫描器时，可直接 `POST /_helmet/unlock`；这仍然只是“隐蔽门”，不是可靠认证。
   - 需要真实保护时，显示密码/一次性码/Passkey/OIDC 入口，并在 `POST /_helmet/unlock` 中完成服务端认证。
4. 门禁服务验证成功后返回 `Set-Cookie`。后续请求由浏览器自动携带 Cookie，代理通过内部鉴权后才转发到 4001。
5. `localStorage` 最多记录“是否显示过机关、点击计数”等 UI 状态；代理授权只相信服务端可验证的 Cookie 或服务端会话。

### 会话 Cookie 的正确形态

可采用以下二选一：

- **不透明随机会话 ID**：Cookie 仅保存高熵随机 ID，会话内容和过期时间保存在门禁服务端。这更容易撤销。
- **签名令牌**：Cookie 内含随机会话 ID、签发时间、到期时间和版本，并用服务端密钥做 HMAC/AEAD；每次请求都验证签名和过期时间。需要密钥轮换和主动撤销时，仍应保留少量服务端状态。

Cookie 应使用类似下面的属性：

```http
Set-Cookie: __Host-id=<opaque-or-signed-value>; Path=/; Secure; HttpOnly; SameSite=Strict; Max-Age=28800
```

`Secure` 限制只经 HTTPS 发送，`HttpOnly` 阻止页面 JavaScript 读取，`SameSite` 降低跨站请求携带会话的范围，`__Host-` 前缀还要求 `Secure`、`Path=/` 且不得设置 `Domain`。OWASP 也建议会话标识具备足够熵、设置这些 Cookie 属性，并避免把凭据放进 `localStorage`。参见 [OWASP Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html) 和 [RFC 6265](https://www.rfc-editor.org/rfc/rfc6265.html)。

如果解锁接口会建立有权限的会话，还应使用同源 `POST`、一次性 CSRF nonce、短期过期、失败限速和审计日志。单纯的点击次数不能代替服务器端凭据。

## URL 放行策略

应该使用“默认保护、显式放行”，按 **host + HTTP method + 完整 path/prefix** 配置，而不是只检查字符串是否包含某段内容。例如只放行：

- `GET /healthz`
- `POST /v1/chat/completions`，但继续要求原服务的 API Key
- `POST /v1/messages`，但继续要求原服务的 API Key
- 真正需要公开的静态资源前缀

不要因为请求“看起来不像浏览器”就放行整个站点。非浏览器客户端应使用 API Key、Bearer token、mTLS 或专门的 service token。若把 API 路径公开给代理层，4001 上原有的 API 鉴权也不能关闭。

还应由代理清理并重建 `X-Forwarded-*` 等身份相关请求头，防止客户端伪造代理身份；内部鉴权端点和解锁端点不得转发到 4001。

## 基础反向代理能力

### NGINX + 小型门禁服务

这是最符合“固定仿 NGINX 404 外观 + 隐藏机关”需求的组合。

- `proxy_pass` 可把请求转发到 `127.0.0.1:4001`；`location = /exact`、前缀 location 等可建立公开路径 allowlist。参见 [NGINX proxy_pass](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_pass) 和 [location](https://nginx.org/en/docs/http/ngx_http_core_module.html#location)。
- `auth_request` 可把每次受保护请求交给门禁服务验证：2xx 放行，401/403 拒绝。该模块需要 NGINX 构建时包含 `--with-http_auth_request_module`。参见 [NGINX auth_request](https://nginx.org/en/docs/http/ngx_http_auth_request_module.html)。
- 门禁服务在无会话时返回 403，主请求再用 `error_page 403 =404 /helmet-404.html` 改成真正的 404；自定义错误页可标为 `internal`，避免被直接当作普通公开资源访问。参见 [error_page](https://nginx.org/en/docs/http/ngx_http_core_module.html#error_page) 和 [internal](https://nginx.org/en/docs/http/ngx_http_core_module.html#internal)。认证子请求不应直接返回 404，因为 `auth_request` 只把 2xx、401 和 403 视作正常鉴权结果。
- `server_tokens off` 只能隐藏版本号，开源 NGINX 仍可能发送 `Server: nginx`；伪装页的价值是减少信息和噪声，不是阻挡漏洞利用。参见 [server_tokens](https://nginx.org/en/docs/http/ngx_http_core_module.html#server_tokens)。

概念配置如下，实际部署时仍需补 TLS、请求头清理、限速和日志：

```nginx
server {
    listen 443 ssl;
    server_name service.example.com;

    # 只放行明确列出的 URL；API 自己仍要校验 API Key。
    location = /healthz { proxy_pass http://127.0.0.1:4001; }
    location = /v1/chat/completions { proxy_pass http://127.0.0.1:4001; }

    # 页面达到机关条件后调用；这里必须由门禁服务决定是否签发 Cookie。
    location = /_helmet/unlock {
        proxy_pass http://127.0.0.1:4002;
    }

    location = /_helmet_auth {
        internal;
        proxy_pass http://127.0.0.1:4002/verify;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
        proxy_set_header X-Original-URI $request_uri;
    }

    error_page 403 =404 /helmet-404.html;
    location = /helmet-404.html {
        internal;
        root /var/www/helmet;
    }

    location / {
        auth_request /_helmet_auth;
        proxy_pass http://127.0.0.1:4001;
    }
}
```

### Caddy + 小型门禁服务

Caddy 也能完成同样工作，并默认提供自动 HTTPS：

- `reverse_proxy 127.0.0.1:4001` 负责上游转发；path matcher 与互斥的 `handle` 可以先匹配公开 URL，再用无 matcher 的 `handle` 作受保护 fallback。参见 [reverse_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)、[request matchers](https://caddyserver.com/docs/caddyfile/matchers) 和 [handle](https://caddyserver.com/docs/caddyfile/directives/handle)。
- `forward_auth` 把请求克隆给门禁服务，2xx 时继续执行，非 2xx 时把门禁服务响应返回给客户端。因此门禁服务可以直接返回 404 伪装页，无需像 NGINX 那样把 403 再映射为 404。参见 [forward_auth](https://caddyserver.com/docs/caddyfile/directives/forward_auth)。
- `respond` 可以直接产生自定义状态码和响应体；复杂静态错误页可使用 `handle_errors` 与 `file_server`。参见 [respond](https://caddyserver.com/docs/caddyfile/directives/respond) 和 [handle_errors](https://caddyserver.com/docs/caddyfile/directives/handle_errors)。

如果只需要少量配置和自动证书，Caddy 更省事；如果必须高度还原 NGINX 行为和页面，NGINX 更直接。

## 成熟方案对比

| 方案 | 反向代理 | 路径放行 | 会话 Cookie | 默认伪装 404 | 开源情况 | 适合场景 |
|---|---|---|---|---|---|---|
| NGINX + 自定义门禁服务 | 是 | `location` | 由门禁服务签发/验证 | 最容易精确实现 | NGINX OSS；门禁服务自建 | 最符合本需求 |
| Caddy + 自定义门禁服务 | 是 | matcher + `handle` | 由门禁服务签发/验证 | `forward_auth` 可直接返回 404 | Caddy OSS；门禁服务自建 | 配置简单、自动 HTTPS |
| Authelia | 通过 NGINX/Caddy 等集成 | `resources`、method、network，支持 `bypass` | 内置服务端会话 | 默认是认证门户/重定向；需代理改写为 404 | [Apache-2.0](https://github.com/authelia/authelia) | 自托管账号、1FA/2FA、Passkey |
| oauth2-proxy | 可自身代理，也可配 NGINX `auth_request` | `skip-auth-route`，多 upstream 按 path 路由 | Cookie 或 Redis session | 默认是登录/401；需代理改写 | [MIT](https://github.com/oauth2-proxy/oauth2-proxy/blob/master/LICENSE) | 已有 Google/GitHub/企业 OIDC，希望轻量接入 |
| Pomerium Core | 身份感知反向代理 | route 的 exact path、prefix、regex 和 policy | 加密签名 Cookie + Databroker session | 默认触发 IdP 登录；不是隐藏机关 | [Apache-2.0](https://github.com/pomerium/pomerium/blob/main/LICENSE) | 多服务、细粒度策略、零信任访问 |
| Cloudflare Access | Cloudflare 边缘 + Tunnel | application path、单独 Bypass policy | `CF_Authorization` JWT Cookie | 默认登录或拦截页，不是 NGINX 404 | Access 是托管服务；只有 [cloudflared 连接器](https://github.com/cloudflare/cloudflared) 为 Apache-2.0 | 不想自托管认证基础设施 |

### Authelia

Authelia 是成熟的自托管认证与授权门户，通过 NGINX `auth_request` 等机制接入反向代理。它的规则可按 domain、path/resource 正则、method、用户/组和网络匹配，并建议使用默认 deny；明确的公开 URL 可设为 `bypass`。参见 [NGINX integration](https://www.authelia.com/integration/proxies/nginx/)、[Proxy Authorization](https://www.authelia.com/reference/guides/proxy-authorization/) 和 [Access Control](https://www.authelia.com/configuration/security/access-control/)。

Authelia 使用会话 Cookie，支持过期、非活跃超时和 Redis，会设置 `HttpOnly`、`Secure`、`SameSite` 等安全属性；官方架构是未登录时跳转认证门户，登录后 Cookie 随内部鉴权请求验证。参见 [Session configuration](https://www.authelia.com/configuration/session/introduction/)、[Security measures](https://www.authelia.com/overview/security/measures/) 和 [Architecture](https://www.authelia.com/overview/prologue/architecture/)。

它适合真正保护管理后台，但不会原生实现“点击 20 次后无凭据解锁”。若仍要 404 外观，需要让代理把未授权结果映射成伪装页，并把机关仅用来揭示 Authelia 登录入口。

### oauth2-proxy

oauth2-proxy 是较轻量的 OAuth/OIDC 认证反向代理。它既能把流量代理到一个或多个 upstream，也能只提供 `/auth` 给 NGINX `auth_request`；`skip-auth-route` 可按 method + path 正则绕过认证。参见 [当前配置参考](https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview/) 和 [NGINX auth_request 示例](https://oauth2-proxy.github.io/oauth2-proxy/7.0.x/configuration/overview/#configuring-for-use-with-the-nginx-auth_request-directive)。

它支持以 Cookie 或 Redis 保存 session，配置 `cookie-secret`、`cookie-secure`、`cookie-httponly`、`cookie-samesite` 和过期时间。默认体验仍是 OAuth 登录或 401，不是伪装 404；外层 NGINX/Caddy 可以改写未授权响应。已有 GitHub、Google、Keycloak 或其他 OIDC 身份提供者时，这是最小改动的成熟方案。

### Pomerium

Pomerium Core 本身就是身份感知反向代理。每条 route 具有 `from`、`to` 和 policy；path 可用 exact、prefix 或 regex 匹配，公开 route 可设置 `allow_public_unauthenticated_access`，其余 route 可要求任意已登录用户或更细的身份策略。参见 [Routes](https://www.pomerium.com/docs/reference/routes)、[Path Matching](https://www.pomerium.com/docs/reference/routes/path-matching)、[Policy](https://www.pomerium.com/docs/reference/routes/policy) 和 [Public Access](https://www.pomerium.com/docs/reference/routes/public-access)。

认证后，Pomerium 在 Databroker 建立 session，并保存引用该 session 的 Cookie；Cookie secret 用于加密和签名 Cookie。参见 [Sessions](https://www.pomerium.com/docs/internals/sessions) 和 [Cookie settings](https://www.pomerium.com/docs/reference/cookies)。它适合多个内部应用和持续策略验证，但配置与组件比单服务门禁更重，且未登录时通常会进入 IdP 流程，不符合“完全看似 NGINX 404”的原生体验。

### Cloudflare Access（对照）

Cloudflare Access 可以按 hostname/path 建应用和策略，更具体的 path 可有独立规则；公开 callback、webhook 或 health endpoint 可建立单独应用并设置 Bypass。参见 [Application paths](https://developers.cloudflare.com/cloudflare-one/access-controls/policies/app-paths/) 和 [Bypass a public endpoint](https://developers.cloudflare.com/cloudflare-one/access-controls/policies/common-policies/#bypass-a-public-endpoint)。

受保护请求必须包含有效的 `CF_Authorization` Cookie，其中是 Cloudflare 签发的 JWT；非浏览器客户端可以使用 Service Auth/service token，而不是伪造 User-Agent。参见 [Authorization cookie](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/authorization-cookie/) 和 [Access policies](https://developers.cloudflare.com/cloudflare-one/access-controls/policies/)。

它部署方便，但 Access 的控制面和边缘判定是 Cloudflare 托管服务，不是可完整自行部署的开源项目；开源的是 `cloudflared` Tunnel 连接器。它也默认展示 Cloudflare 登录/拦截体验，不满足伪装 NGINX 404 的外观，除非额外增加 Worker/源站代理层。

## 最终建议

按目标分两种实施等级：

1. **只想减少公网扫描噪声和偶然发现**：使用 Caddy 或 NGINX + 一个很小的门禁服务。保留 20 次点击机关，但由门禁服务签发短期 `HttpOnly` Cookie；明确写入威胁模型：查看页面源码或复现解锁请求的人可以绕过。
2. **希望真正防止攻击和未授权访问**：仍可保留 404 伪装和 20 次点击，但点击后必须出现真正认证。单用户、小规模优先 NGINX/Caddy + 密码/Passkey 门禁；需要自托管 MFA 选 Authelia；已有 OIDC 选 oauth2-proxy；有多个内部服务和复杂策略选 Pomerium；愿意使用托管服务则选 Cloudflare Access。

对当前“一个 4001 服务、少量 URL 可公开、外观必须像 NGINX 404”的描述，首选是：

> **NGINX + 独立轻量门禁服务 + 服务端短期会话 Cookie + 默认 404 + 精确 URL allowlist**。

不要实现“非浏览器 User-Agent 自动放行”，也不要让 `localStorage` 决定是否代理。即使保留机关，它也应只是认证入口的视觉隐藏层。
