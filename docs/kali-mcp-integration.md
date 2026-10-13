# Kali Linux MCP Gateway 集成

## 架构与安全边界

`ARTEX Agent → Skill/DeferredInfo → ExecuteExtraTool → 现有 MCP HTTP 客户端
→ HTTPS + Bearer Token → kali-mcp 容器内的上游 Gateway → Kali 工具`。

- 基础 docker-compose.yml 和 Dockerfile 不改；需显式添加可选 overlay。
- 默认关闭，无自动 Agent/Skill 授权。保留 Planner/Worker 双图、事实记录、
  漏洞登记、工具拦截、其他 MCP 和现有 Skills。
- `kali` 是本集成保留的 MCP 名称。自动注册是 insert-if-absent，不覆盖
  同名已有配置、用户禁用、CA、URL 或 Agent 绑定。Token 只存显式引用。
- 任意 Agent（包含 planner/worker/pentest）必须被分配 kali-mcp Skill；
  仅直接绑定 kali MCP 不足以获得工具。Skill 加载后才解锁，schema 仍按需搜索。
- 每次 Agent 装配建立独立 MCP 会话；不跨 Worker 共享 Session ID，结束 DELETE。
- JSON/SSE、初始化协商版本、分页 tools/list、JSON-RPC 错误、isError、超时
  通过既有客户端处理。工具原名保留，外部名是 `mcp__kali__<上游原名>`。
- 工具调用走既有权限/工具执行层、活动记录和 tool_usage 账本；
  server log 增加名称、成功/错误和耗时，不记录参数、认证信息或结果正文。
- 工具文本不自动转成漏洞。继续由 ARTEX 的事实/漏洞登记机制确认证据。

## 环境与资源

需要 Linux Docker Engine、Compose >=2.24（隔离测试 overlay 的 !reset/!override）。
上游镜像很大：amd64 压缩下载约 6.9GB，实际空间取决于版本和存储驱动。
containerd image store 同时保留压缩层与解压快照，建议**至少 30GB 可用空间**，
不能仅按压缩大小估算。完整工具集建议宿主至少 8GB 可用 RAM，
默认 Gateway 上限 6GB、/dev/shm 512MB；实测多个并发初始化在 3GB 限额下
会出现明显内存压力和超时。限制是上限，不是启动即占满。
Burp/Chromium/并行扫描会额外耗内存，需单独调整。

独立 Bridge 默认不公布任何端口，无 host 网络、Docker socket、privileged。
NET_RAW 已显式启用，NET_ADMIN 不启用；允许扫描的目标取决于网络范围和管理员授权。
如确实需要 NET_ADMIN，由管理员写单独覆盖文件 cap_add: [NET_ADMIN]，
禁止为方便测试改成 privileged 或 host 网络。默认不运行 Burp。
上游 Nmap 二进制的文件 capability 包含 NET_ADMIN，超出容器 bounding set
时会导致 exec 返回 Operation not permitted。入口 wrapper 仅清除该文件的
capability，使用 root 进程已有的 NET_RAW；不增加 NET_ADMIN，不关闭
no-new-privileges，不改上游工具实现。

## 启用（从仓库根目录）

1. 使用包含本次修改的镜像（旧发布镜像不会自动获得新代码）：

   ```sh
   cp .env.example .env
   # 编辑 POSTGRES_PASSWORD；设置 KALI_MCP_ENABLED=true
   mkdir -p secrets/kali-mcp
   chmod 700 secrets/kali-mcp
   (umask 077; openssl rand -hex 32 > secrets/kali-mcp/token)
   ```

2. 启动独立 Gateway；此时 ARTEX 尚不需要重启，且 Gateway 未公布宿主端口：

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.kali-mcp.yml --profile kali up -d kali-mcp
   docker compose -f docker-compose.yml -f docker-compose.kali-mcp.yml --profile kali logs kali-mcp
   # 等 TLS 生成完成，再复制公共 CA；不要复制 server.key！
   docker compose -f docker-compose.yml -f docker-compose.kali-mcp.yml --profile kali cp \
     kali-mcp:/opt/kali-mcp/ssl/service_ca.crt secrets/kali-mcp/service_ca.crt
   chmod 644 secrets/kali-mcp/service_ca.crt
   ```

3. 在计划升级的部署中构建并启动 ARTEX（不是在正在运行的原生部署上强制覆盖）：

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.kali-mcp.yml --profile kali build artex
   docker compose -f docker-compose.yml -f docker-compose.kali-mcp.yml --profile kali up -d artex postgres
   ```

无 depends_on: kali-mcp，因此 Gateway 不可用不阻止 ARTEX 启动。
首次启动异步 seed/discovery，之后每两分钟重新发现；用户禁用时不连接。
单次初始化/发现有超时，不会无限阻塞启动；Agent 新建运行时也会重连。

## 配置与 Secret

| 参数 | 默认/作用 |
|---|---|
| KALI_MCP_ENABLED | false；ARTEX 端总开关，容器启用仍需 --profile kali |
| KALI_MCP_IMAGE | vxcontrol/kali-linux:mcp；建议生产固定经验证 digest |
| KALI_MCP_URL | https://kali-mcp:8081/mcp |
| KALI_MCP_SECRET_DIR | ./secrets/kali-mcp；只读挂载 |
| KALI_MCP_TOKEN_FILE | /run/secrets/kali-mcp/token；优先于环境变量 |
| KALI_MCP_TOKEN | 可选替代环境变量，文件模式下留空 |
| KALI_MCP_CA_CERT | /run/secrets/kali-mcp/service_ca.crt |
| KALI_MCP_DISABLE | 逗号分隔上游子服务名；未知名称由上游拒绝 |
| KALI_MCP_SHM_SIZE / MEMORY | 512m / 6g |
| KALI_MCP_BURP_ENABLED | false |
| KALI_MCP_BURP_ACCEPT_EULA | false；管理员明确接受 PortSwigger EULA 才设 true |

MCP env 中 HTTP Header 支持 `${env:VARIABLE}`、`${file:/绝对路径}`，
例如 `{"Authorization":"Bearer ${file:/run/secrets/kali-mcp/token}"}`。
引用在连接时解析，不写回数据库；不可读、未设置、空值、CR/LF 返回安全错误。
不使用隐式 shell expansion。不要把 Token 写入 Git、工具参数、日志或文档。
文件模式避免 docker inspect 暴露明文；上游自身可能把 Token 放入进程环境，
因此持有容器/root 权限仍视为持有全部 Gateway 权限。

环境变量替代方式：KALI_MCP_TOKEN_FILE 设为**空字符串**、填写 KALI_MCP_TOKEN；
这会暴露给 docker inspect，不推荐。本 overlay 缺 Token 也可解析，
只有显式启动 Gateway 时才由上游拒绝缺 Token。

CA 是按连接的 trust pool，保持系统根证书不变，不使用跳过校验。
MCP 的 ca_cert 字段支持文件路径/显式引用；可在系统 → MCP 页面编辑。
Kali 强制 verified HTTPS，不允许 insecure=true。自动生成证书 SAN 含
kali-mcp；更换域名必须生成/提供相应 SAN 的证书，不能跳过校验代替。
仅 Gateway 挂载 TLS 私钥卷，ARTEX 只得到复制的公共 CA。
如果用户已编辑过同名 MCP，环境变量不覆盖原配置；在 MCP 页面显式更新即可。

## Agent 授权与工具查看

系统 → MCP 找到 kali，启用并“刷新工具”；工具列表来自实际 tools/list。
系统 → Skills 为指定 Agent 分配 kali-mcp；不要默认授权全部角色。
Agent 加载 Skill 后按关键词 SearchExtraTools，按真实名称/schema ExecuteExtraTool。
已运行会话的可见性遵循原有装配生命周期，撤销权限后应停止旧会话再启动。
数据库迁移仅新增 ca_cert 列，重复安全，不重置 Prompt、Skill 或绑定。

## 子服务、Burp、流量

例如 KALI_MCP_DISABLE=metasploit,playwright 可减少资源；重建 Gateway 配置后
MCP 刷新或定时发现同步新列表，数量不写死。不要复制上游整个 MCP 源码。
Burp 需管理员同时设置两个 BURP 参数为 true；wrapper 对缺少 EULA 接受时拒绝启动。
上游 Community 工件已包含在镜像中不等于接受 EULA，也不等于默认启动。
不要公布 Burp/metrics/admin/readiness 端口。

Kali HTTP 流量不会自动进入 ARTEX 的本地录制代理；两者在不同容器。
现有 ARTEX 流量功能保持原样。如确需记录 Kali 流量，需针对对应工具
配置合法代理与 CA；本集成不全局劫持 Kali 网络，也不降低 TLS 安全性。

## 关闭、升级、回滚

- 在 MCP 页面禁用 kali 即停止新发现/新装配；不影响其他 MCP。
- KALI_MCP_ENABLED=false 后重建该部署的 ARTEX 环境；
  `docker compose ... stop kali-mcp` 停独立容器。不开 profile 时不会新启动它。
- 升级前备份 PG、Kali work/output/state/ssl 卷和公共 CA；
  固定 KALI_MCP_IMAGE 为验证过的 digest，先跑隔离验收再升级部署。
- 回滚 ARTEX 镜像并停用 Kali，新增数据库列可保留，旧代码忽略它。
  上游镜像回滚须考虑工具状态格式；不要直接删除卷。
- 不将容器测试二进制拷到现有 Windows 原生安装。

## Windows 原生 ARTEX + WSL 原生 Kali（无 Docker 运行时）

Kali 工具链不是整体可移植的 Windows 程序：原始套件依赖 Linux ELF、
Python/系统包、浏览器及 Metasploit RPC。推荐 Windows ARTEX 通过 HTTPS MCP
连接 WSL Gateway，而不是逐个从 Windows 执行 wsl.exe。Windows ARTEX
不需要 Docker、Kali 包或 Linux Go 编译器；Gateway 运行在独立 Kali 发行版。

本次使用与容器验收相同的上游镜像 rootfs 导入 WSL，保持工具版本一致。
导入不是容器运行：Gateway/工具是原生 Linux 进程，目标发行版无 Docker daemon
和 CLI。镜像 rootfs **不等于官方完整 WSL 安装包**，需手动补齐 systemd/dbus，
恢复 image ENV、PATH 和 HOME；自行从普通 Kali 安装上游 MCP 是另一条路径，
本次没有验证该安装方式。测试的镜像 digest：
`vxcontrol/kali-linux@sha256:99cd09d8580fe26e9a22f689497c858b27447e27e2b972de03ab2192a850ad65`。

### 准备发行版（仅在专用实验目录执行）

如果已有经验证的 rootfs tar 和 Config.Env JSON，可直接导入；否则在
**独立的 Linux Docker 构建环境**导出（工具发行版之后无需 Docker）：

```sh
image=vxcontrol/kali-linux@sha256:99cd09d8580fe26e9a22f689497c858b27447e27e2b972de03ab2192a850ad65
docker pull "$image"
docker image inspect --format '{{json .Config.Env}}' "$image" > kali-image-env.json
id=$(docker create "$image")             # 不启动上游入口
docker export -o kali-native-rootfs.tar "$id"
docker rm "$id"
```

Windows PowerShell，发行版名称/目录必须是新的，不能覆盖现有发行版：

```powershell
wsl --import ARTEX-Kali-Native X:\WSL\ARTEX-Kali-Native X:\WSL\Artifacts\kali-native-rootfs.tar --version 2
```

在目标 WSL 内安装 systemd/systemd-sysv/dbus，在 /etc/wsl.conf 设置
`[boot] systemd=true` 后手动 terminate/restart。禁用该 rootfs 中不需要的
ssh/apache/postgresql/docker/containerd 等服务；不 enable Gateway 或 ARTEX
开机启动。可移除导入镜像中的 docker-ce-cli，不在这里安装容器引擎。

在 WSL 内将可信 image-env JSON、仓库 deploy/kali-mcp-native.py 分别复制至
`/opt/artex-kali-gateway/image-env.json` 和 `/opt/artex-kali-gateway/`，创建
权限 700 的目录及权限 600 的随机 token 文件。不要复制 TLS 私钥到 Windows。

**启动前隔离端口**：上游 Gateway/管理组件不保证全部遵守 MCP_GATEWAY_IP，
不能仅凭该变量判断为 loopback-only。用专用防火墙链限制 18081、15000、
15020、15021：允许 lo；Windows NAT 场景仅允许宿主侧网关访问 18081；
其他来源拒绝；IPv6 仅允许 lo。用 `ss -lntp` 检查实际监听。不要 flush 全局
iptables 或干扰现有 Docker/其他发行版规则，注意 WSL 发行版可能共享网络。
原生进程没有容器 capability/文件系统隔离，root Gateway 可访问 WSL 挂载的
Windows 文件；应使用专用可信发行版、限制挂载/入站范围和 Agent 授权。

### 手动启动和连接

```sh
systemd-run --unit=artex-kali-native-gateway --property=MemoryMax=6G \
  --property=TasksMax=512 /usr/bin/python3 /opt/artex-kali-gateway/kali-mcp-native.py \
  --image-env /opt/artex-kali-gateway/image-env.json \
  --token-file /opt/artex-kali-gateway/token --port 18081
# 等 readiness 和 Metasploit RPC 就绪，之后在 ARTEX 刷新工具
curl -fsS http://127.0.0.1:15021/healthz/ready
ss -lnt | grep ':55553 '
```

这是 transient unit，不是 enable 的启动服务；stop 后可能被 systemd 收集，
再次启动需重跑 systemd-run，而不是 systemctl start 不存在的单元。
默认 Burp 始终关闭。launcher 只启动原有上游入口，不复制/重写 MCP 服务。
CA 生成于 `/opt/kali-mcp/ssl/service_ca.crt`，将**公共 CA**与相同 token
分别安全复制到 Windows 测试目录。Windows 进程使用 Windows 文件路径：

```powershell
$Lab = 'X:\ARTEX-Kali-Test\Windows'
# ARTEX_PG_DSN 应先指向独立测试 PG/数据库，不能使用生产 5432
$env:KALI_MCP_ENABLED = 'true'
$env:KALI_MCP_URL = 'https://localhost:18081/mcp'
$env:KALI_MCP_TOKEN_FILE = "$Lab\secrets\kali-token"
$env:KALI_MCP_CA_CERT = "$Lab\secrets\kali-ca.crt"
$env:ARTEX_SKILL_DIR = 'X:\ARTEX-Kali-Test\source\skills'
& "$Lab\artex-test.exe" -addr '127.0.0.1:19088' -data "$Lab\data" '-proxy='
```

本机 WSL localhost relay 实测只在 `::1:18081` 接受连接，Windows 的
`127.0.0.1:18081` 会被拒绝；因此使用 **localhost**，证书 SAN 必须包含
localhost。不要为修复 IPv4 relay 关闭 TLS 或将 Gateway 公布到 LAN。
网络模式变更后重新验证 relay/防火墙/证书；本次未更改用户 .wslconfig。
新测试程序、数据目录、JWT、PG 端口/数据库都需与生产独立，绝不能覆盖
现有 X:\ARTEX 或把生产 DSN 交给测试。系统 → MCP/Skills 的配置与容器相同。

## 同一 Linux 环境的原生 ARTEX + Kali Gateway

使用上述原生 Gateway，不启动 Docker；Linux ARTEX 作为另一个原生进程
使用 `https://localhost:18081/mcp`、Linux token/CA 绝对路径和独立 PG。
编译需要项目 go.mod 指定的 Go 版本；编译后的程序不依赖 Go 运行时。

```sh
# 在源码根编译；如需 UI，先静态构建前端并复制到 server/webui/dist
CGO_ENABLED=0 go build -tags embedui -o /opt/artex-test/artex ./cmd/artex
export KALI_MCP_ENABLED=true KALI_MCP_URL=https://localhost:18081/mcp
export KALI_MCP_TOKEN_FILE=/opt/artex-kali-gateway/token
export KALI_MCP_CA_CERT=/opt/kali-mcp/ssl/service_ca.crt
export ARTEX_SKILL_DIR=/opt/artex-test/source/skills
# ARTEX_PG_DSN 由管理员安全设置为独立测试数据库
cd /opt/artex-test
./artex -addr 127.0.0.1:19087 -data /opt/artex-test/data -proxy=
```

本次 Linux 原生路径在 WSL2 Kali Linux 中实测，不代表在所有物理 Linux
发行版/内核、无线网卡 monitor mode、GPU 驱动上通过。无容器时入口中的
NET_ADMIN 限制不适用，需要管理员另外配置 OS 权限边界。
Windows 证据文件保留正文 fsync 和同卷 atomic rename，但不能像 Unix
一样用 Go 只读目录句柄做目录 fsync；元数据掉电持久性保证与 Unix 不同。

测试结束手动 stop Gateway/测试 ARTEX/测试 PG，随后 Windows 执行
`wsl --terminate ARTEX-Kali-Native`。保留发行版/数据，不 unregister；
无任何 Windows 开机自启。本机既有无限闲置配置下不能依赖 WSL 自动退出。

## 验证

```sh
go test ./...
go vet ./...
(cd norma && go test ./...)
go test -race ./mcphttp
POSTGRES_PASSWORD=test docker compose -f docker-compose.yml -f docker-compose.kali-mcp.yml config --quiet
bash tests/kali-mcp/run.sh
```

全套 Go 回归需要独立 PG 和可执行的 Python（部分已有测试不会 skip 缺 DB）。
不要用生产 DSN 跑测试。run.sh 在专用 artex-kali-lab 项目内构建真实 ARTEX 后端，
使用真实 vxcontrol 镜像、内网 fixture、独立 PG，不带 LLM 密钥、不公布端口。
Windows 可交叉编译 linux/amd64 的 artex、server.test、db.test、mcphttp.test
放入 tests/kali-mcp/artifacts 后，在 Linux Docker 主机运行。

脚本验证 401、初始化/Session、全部要求的工具类别、Skill 锁/可见性、
单个 fixture:8000 的低影响调用经 ExecuteExtraTool 返回和账本、
多会话并发、网关停止后 ARTEX 健康/冷启动、重启再发现。
不调用公网扫描、不启动 Burp；不代表所有工具的每一种操作已验证。
仅退出 0 才算真实镜像验收通过。输出在 artifacts（不入 Git）。

原生复用同一组集成测试，在独立测试数据库及 loopback fixture:8000 下设置：

```sh
export KALI_TEST_MODE=native KALI_TEST_TARGET=127.0.0.1
export KALI_TEST_URL=https://localhost:18081/mcp
export KALI_TEST_SKILL_DIR=/opt/artex-test/source/skills
export KALI_TEST_ARTEX_URL=http://127.0.0.1:19087
export KALI_TEST_JWT_FILE=/opt/artex-test/jwt.key
go test ./server -count=1 -run '^TestKali(DockerGateway|RunningARTEXAPI)$' -timeout 10m -v
# 停 Gateway 后设置 KALI_TEST_GATEWAY_DOWN=true，执行
# '^TestKali(DockerUnavailable|RunningARTEXAPI)$'；然后重启 ARTEX 验证冷启动，
# 恢复 Gateway/RPC 就绪后再执行正向测试。
```

Windows 使用相同变量含义、Windows 文件路径、19088 测试 API 端口。
相关测试会暂时改**实验数据库**里的角色绑定，完成后恢复；不得指向生产库。

## 故障排查

- secret reference empty/unavailable：检查 ARTEX 和 Gateway 两端的挂载与容器内文件，
  不要用日志打印 Token 排查。
- 401：确认两端读同一 Token，避免末尾额外空格；新会话重新初始化。
- x509 / CA unavailable：核对公共 CA、SAN、容器内文件路径。TLS 卷更换后重新复制 CA。
- 连接拒绝：查 Gateway 日志、资源/OOM、Docker DNS、私有 Bridge；ARTEX 应保持可用。
- 工具缺失：查 KALI_MCP_DISABLE 和 Gateway 真实工具列表，不以 126 等数量判断成功。
- 工具不可用/locked：确保 Skill 分配给当前 Agent 且已经加载；直接绑定不能绕过 Skill。
- ENOSPC：预留解压快照、压缩层、构建和日志空间，不能只清几十 MB 或按压缩体积估算。
  不自动清理其他部署；磁盘不足时先停止拉取、扩容或由管理员清理。
- 大型工具错误/超时：结果按既有 Capture 截断/溢写，客户端默认 120 秒请求上限；
  长任务优先使用上游提供的异步操作及结果查询，不自动无限延长全局超时。

## 当前交付验证状态

2026-10-13 已实测 Windows 原生 ARTEX + WSL 原生 Gateway、同一 WSL Linux
原生 ARTEX + Gateway，以及隔离真实 Docker 镜像路径。三条路径均通过正确
Token/CA、无/错 Token 401、所有要求类别发现、三角色 Skill gating、单端口
Nmap 调用与审计、三独立会话并发、网关停止/ARTEX 冷启动/恢复再发现。
发现数量为当时的 126，不用固定数量作为长期验收标准；未逐项执行全部工具，
未验证 Burp/GPU/真实外网扫描，没有用真实 LLM 密钥跑自动 Pentest。

Windows 与 Linux 全量 Go 串行回归存在一个基线已有的失败：
`agent/TestGraphOverviewExpandsAssociatedCompanyScope` 期望 coverage.scope，
当前 compact overview 不返回该字段；相关 Agent 源码与测试本次未修改。
已在 e6233f5 单独复现，不为了本集成改变既有 Agent 上下文行为；其余主模块
包通过。Linux `go vet ./...`、`go test -race ./mcphttp` 和前端 TS 检查通过。
嵌套 norma 模块全量测试另有 llm/noa/noaadapter 的既有失败，相关源码本次
未改，重试/增长阈值/抑制/分层相关确定性失败也在 e6233f5 复现。
因此**不声明全仓库测试全绿**。详细回归结果见交付报告；这些测试程序未
覆盖/升级现有生产部署。
