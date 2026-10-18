# 外部 MCP 接入

ARTEX 只管理 MCP 连接、工具发现和 Agent 可见权限，不负责安装或维护 Kali、浏览器等外部服务。

1. 在独立机器、WSL 或自主管理的容器中运行外部 MCP，按照其自身文档配置认证和网络访问。
2. 在「系统 → MCP」新建连接。远端服务使用 Streamable HTTP 或 SSE，填写服务实际提供的 URL；本地子进程使用 stdio 命令及参数。
3. HTTP 认证头可引用 `${env:MCP_TOKEN}` 或 `${file:/absolute/path/token}`，避免把密钥写进仓库。自签名服务填写专用 CA 路径，保持证书验证。
4. 启用连接并点击「刷新工具」。只有真实 `tools/list` 返回的工具会出现在 GUI 中。
5. 在 Agent 管理中明确分配该连接；也可以由自定义 Skill 引用 MCP 名称，按需加载工具。不会默认对所有 Agent 授权。
6. 从相应 Agent 的新会话测试调用，确认其可见权限、服务认证和网络路径正确。

Kali 不再是保留名称，也不再受 `KALI_MCP_ENABLED`、内置 Skill 或后台专用轮询控制。已有手工 MCP 记录保留，不自动删除或改写。旧部署可以移除 Kali 专用 Compose overlay 和环境变量，但不要删除仍在使用的外部服务凭据或数据。
