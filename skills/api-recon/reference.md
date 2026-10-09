# api-recon — Reference Manual

Grep recipes, a `config.json` template, and troubleshooting. All greps are run against the `js/` directory. For single-line bundles, run `js-beautify` or `sed 's/}/}\n/g'` first if needed — usually a raw grep with a context window is enough.

## About the scripts

Everything in `scripts/` is a **reference template** — it must be tuned per target site before running. Typical changes:

| Script | Common tuning points |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success value, stub matching rules and body shape, routes source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether to enable L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` destructive links, cookie, depth/max, same-origin filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field names |
| `config.json` | the single entry point for all of the above site-specific parameters |

Put the tuned files in the task's working directory (e.g. `recon/`), and note in the report exactly what changed relative to the reference scripts.

---

## A. The three reversal gates

### A1. Render gate — "How does it know you're logged in?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Find the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))`, and pin down the **storage key**, the **container** (cookie vs localStorage), and the **encoding**:

| Encoding | How to forge it in config |
|---|---|
| plain string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | an unsigned/`alg:none` JWT, or sign with a key found in the bundle |
| encrypted (SM2/AES/RSA) | look for a hardcoded key; forge is possible if the render gate only needs a decodable blob, otherwise fall back to static recon |

→ write into `cookies` / `localStorage`.

### A2. Interceptor gate — "What triggers the redirect to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```
(Note: the third grep's pattern intentionally searches for Chinese auth-failure phrases — "not logged in" / "please log in again" / "login expired" / "authorization" — since many pentest targets have Chinese-language UIs. Adjust it to the target's actual language.)

Determine: the **field name**, the **success value** (usually `0` or `200`), and the **failure value that triggers the redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ write into `neutralize.fields` + `neutralize.success`.

### A3. Content gate — "Where do the menu/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common in enterprise admin panels):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | route guard, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | sidebar menu rendering |
| `userRouteAuth` in the bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` in the bundle | `{ KEY: { name, link } }` | alias resolution (webpack `o.DASHBOARD`) |

Read the consuming code to confirm: how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): build a permissive payload → `stubs`.

**Full permission-tree reconstruction** (large sites, where the sidebar/submodules are still blank): see **Section I**.

---

## B. config.json template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field notes:
- `runtimeMode`: `depth` (Puppeteer), `coverage` (browser MCP), or `both`
- `cookies[].value` prefix: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal value
- `forward: true` forwards the real request and rewrites the code field; `false` is fully offline stubbing
- `mockTier`: which preload tiers are active in coverage mode, e.g. `L1+L2`, `L1+L2+L3`
- `routes` comes from `routes.txt`; once the menu is forged, the harness auto-appends discovered `<a href>` links
- `captureResponses` / `recordWs` only apply in depth mode
- `waitUntil`: use `domcontentloaded` for large SPAs, to avoid `networkidle2` hanging
- `routeTimeout`: per-route `page.goto` timeout (ms)
- `proxy`: Puppeteer's `--proxy-server`; can also set `HTTP_PROXY` / `HTTPS_PROXY`

### B1. Dual-stub template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

The outer field names (`response_code` / `code` / `data`) must match what A2's interceptor gate expects; `permissions` must cover every leaf code in the tree.

---

## C. Coverage mode: preload configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or swap it in before CDP injection:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify with: `window.__API_RECON_PRELOAD__ === true` and a stable pathname.

Export the recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime hook capabilities

Browser hooks built into preload (coverage) and runtime_harvest (depth), and their coverage:

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | records request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | discovers Authorization and other headers | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | confirms session key names | ⚠️ optional via `observe.storageReads/cookieReads` |
| Vue route retrieval | fills in frontendRoutes | ✅ `__API_RECON_ROUTES__` (already-loaded routes) |
| Vue router-guard neutralization / login-redirect suppression | opens up modules to trigger their APIs | ✅ `neutralizeVueRouter` + native navigation neutralization |
| React route retrieval | fills in routes | ⚠️ static analysis + clicking; no dedicated hook |
| Page-navigation suppression (login path) | stays on page for analysis | ⚠️ only suppresses the login path, to avoid blocking normal business navigation |
| Hook crypto libraries (CryptoJS/SM, etc.) | encrypted params → plaintext API body | ❌ requires manually hooking the crypto function's input args; write the finding into config |
| Anti-debugging bypass | otherwise runtime can't record any API | ❌ requires manual handling; static analysis still works |

---

## E. Endpoint-extraction regex (when static results are too sparse)

Loosen `extract_endpoints` in `harvest_static.py`, or run manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → fix |
|---|---|
| Very few static APIs | endpoint dialect mismatch → loosen the regex (section D) |
| chunk count ≪ manifest count | CSS-only or undeployed chunks; 404s are already retried |
| Runtime still shows the login page | render gate is wrong → recheck A1: key name, container, encoding, domain |
| Past the shell but modules are blank | content gate → forge the menu (A3); the `routes` paths may be wrong |
| Each route only produces bootstrap/locale calls | permission codes incomplete → section I's permission-tree reconstruction; check the dual `role_permissions` + `permissions/all` stub |
| Sidebar has items but sub-pages are blank | tree is missing an intermediate node, or codes don't match `userRouteAuth` |
| Every API redirects to login | interceptor gate → verify `neutralize`; nested fields need extended walk logic |
| 0 WS frames | subscription happens only after user interaction; lengthen `perRouteMs` |
| Empty response body | real responses only exist with `forward: true` |
| Chromium missing | install chromium or set `config.chromium` / `CHROMIUM` |
| Mocked a lot but still redirects to login | hook ran too late, or missing a `location.href` setter → use document-start + preload |
| List is completely empty | an empty array from L3 is normal; keep clicking tabs/settings/detail |
| Mistaking a Redux action for a route | filter out internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload isn't running at document-start → change the injection timing, or manually clear the guard if `neutralizeVueRouter: false` |
| Response contains a URL but it never made it into the log | enable `extractUrlsFromResponse`; or extract manually from `__API_RECON_DETAIL__` |
| Don't know the Authorization header name | enable `observe.xhrHeaders`, or check request headers in DevTools |
| Runtime is extremely slow / times out | switch to `waitUntil: domcontentloaded`; lower `routeTimeout`; avoid `networkidle2` |
| Proxy connection fails | check `proxy` / the environment variables; make sure Puppeteer and curl use the same proxy port |

---

## G. Hardened targets

When the server progressively validates the session (an unforgeable signed cookie, or server-rendered menus that can't be stubbed), runtime will get stuck at the shell. Expected behavior:

- **Static analysis is enough for endpoint enumeration** — module paths are in the code
- If authorized, run the same harness with a **real session**: `forward: true`, no neutralize needed, and capture the real methods/params/responses

---

## H. Single-task checklist

1. Confirm the authorization scope
2. **Read** `scripts/harvest_static.py` → tune for the target → run it → review `api_static.txt` and `routes.txt`
3. **Phase 1b**: expand the window around path anchors + binding layer → `param_candidates.json` (section J)
4. Reverse A1/A2/A3 → write a site-specific `config.json`
5. **Read and tune** `runtime_harvest.js` / `preload.js` before running them
6. `runtimeMode=depth`: `npm install` → run the tuned harvest script
7. `runtimeMode=coverage/both`: inject the tuned preload at document-start → enumerate dynamically via browser MCP + a **parameter-triggering matrix**
8. Module not rendering → **section I's permission-tree reconstruction** → patch the stubs → rerun
9. Diff multiple parameter samples + infer from errors → `params_merged.json`
10. Merge into `site_map.json` + `api_merged.txt`, and honestly note coverage, gaps, and what was changed in the scripts

---

## I. Permission-tree reconstruction (Phase 4 deep dive)

Use this when forging a simple `menus: [{ path, show: true }]` isn't enough and submodules still won't mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: the **permission API path**, the **response field names**, and the **consuming chunk's filename**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# produces recon/route_map.json
```

If you see `[!] no routeMap pattern found`: loosen the regex in `extract_route_map.py`, or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build the permission tree + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

What the script does:
1. Parses `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases like `He=o.DASHBOARD`)
2. Resolves aliases → real paths using `route_map.json`
3. Infers the parent from the code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Outputs `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, automatically writes `config.json`'s `stubs` and extends `routes`

**Tune per target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes that aren't related by prefix

### I4. Verify stub consistency

```bash
# permissions count should be ≈ the number of userRouteAuth entries
wc -l recon/perm_codes_all.txt
# routes should cover every link in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# compare runtime_api.json counts before/after forging; check whether modules like /attack, /asset now show API calls
```

| Before forge | After forge (success) |
|---|---|
| same 3–5 bootstrap calls on every route | different routes trigger different module APIs |
| only `/api/locale/language` | module endpoints like `/api/web/...` appear |
| a handful of routes in `routes.txt` | 80–110+ `routes` from route_map |

### I6. If it still fails

- **Coverage mode**: click sidebar items + tabs — permission gating may only fire requests after interaction
- **Stub fields**: compare the real API (curl + real session) against the stub's nesting
- **Extra guards**: grep for button-level checks like `hasPermission|checkRole|func.`, and extend `role_permissions.permissions`
- **Static fallback**: module API paths are still in `api_static.txt`; runtime only needs to fill in METHOD/body; keep `param_candidates.json` and already-recorded samples for params

---

## J. Parameter reversal (Phase 1b / 5b / 5c)

**A methodology, not a generic script.** Find paths with regex; find parameters via anchor-window expansion + UI binding chains + multi-sample diffing + inferring from errors.

### J1. Anchor-window expansion — find the request-building object from a path

```bash
# anchor on a path already known from Phase 1
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper layer and transport shape

```bash
# axios / a shared request wrapper
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation gate — required / format / enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding layer — form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Fill gaps at runtime: in DevTools → Network → request → **Initiator** (call stack), trace upward from `fetch`/`send` to find the request-building function.

### J5. Encrypted parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Don't guess field names from ciphertext** — hook the crypto function's **input arguments** and record the plaintext payload before encryption; write the finding into `config.json` / `param_candidates.json`.

### J6. Parameter-triggering matrix (required in Phase 3)

For each module, record once per operation and diff the request body/query:

| Operation | What to watch for |
|---|---|
| list first load | default pagination values |
| search | keyword, filters |
| advanced filter | optional fields |
| create/edit | the full entity |
| batch/export | `ids[]`, `exportType` |
| sort/paginate | `sortField`, `order` |

Produces `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence rules

| Confidence | Condition |
|---|---|
| **High** | static call-site + ≥2 consistent runtime samples |
| **Medium** | static only, or exactly 1 runtime sample |
| **Low** | inferred from a response/error, not verified a second time |
| **Pending trigger** | field is known statically, but the UI/permission path to reach it hasn't been exercised |

### J8. Quick scenario playbook

| Scenario | Order of operations |
|---|---|
| REST list page | J1 find the request-building object → J6 four-way diff → J3 validation rules |
| create/edit form | J3 Form field names → J4 submit chain → runtime submit + deliberately leave a field blank to see the 400 |
| GraphQL | J2 variables declaration → runtime-record variables per operation |
| encrypted body | J5 hook input args → the pre-encryption fields are the real params |

### J9. Mapping to the api-recon phases

| api-recon phase | parameter recon |
|---|---|
| Phase 1 static | J1 anchor-window expansion |
| Phase 2 / A2 interceptor | globally injected fields (tenantId, sign) |
| Phase 3 runtime | J6 triggering matrix + `param_samples.json` |
| Phase 4 permission tree | different modules have different forms — full fields only trigger once permissions are sufficient |
| Phase 5 merge | `params_merged.json` + confidence; never decide "required" from a single sample |

### J10. Troubleshooting

| Symptom | Fix |
|---|---|
| Field name known statically but never seen at runtime | mark it "pending trigger"; fill in the permission tree / click advanced filters / exercise every option of a linked select |
| Same path, different body shapes | normal — record separately by `action`, don't force a single merged schema |
| Stub response is fake but you want to see the real params | **look at the outbound request** body/headers, don't infer from the stub response |
| 400 reports a nested field | watch for an outer wrapper like `data`/`bizData`/`variables` |
| GraphQL only shows the operation name | expand the `variables` JSON; find `$var: Type` statically |

---
