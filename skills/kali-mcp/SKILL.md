---
name: kali-mcp
description: 在明确授权的隔离实验或安全审计中按需发现和调用独立 Kali MCP Gateway 工具。
mcps: kali
---

# Kali MCP（显式授权、延迟加载）

仅在管理员将本 Skill 分配给当前 Agent 且启用 kali MCP 时可用。
先确认目标、允许的操作和速率；没有明确授权范围时停止并询问。
禁止针对公共互联网或教育/政府目标做自动化测试，不得绕过 ARTEX 拦截规则。

## 发现与调用

本 Skill 加载会解锁 `mcp__kali__*`，但不会向模型一次性注入所有 schema。
1. 用 `SearchExtraTools` 搜索一种当前需要的类别（例如 nmap、whatweb）。
2. 阅读返回的真实名称和 schema。以 Gateway 的 tools/list 为准，不假定数量、参数或名称。
3. 用 `ExecuteExtraTool` 按该名称和 schema 调用。别直接用 Bash 调用未安装的 Kali 命令。
4. 明确区分工具错误、超时、尚在运行与成功；需要时查询对应工具的结果。

## 选择工具

- 网络/指纹：nmap、masscan、whatweb；优先单目标、单端口 TCP connect 低影响检查，不能直接大范围扫描。
- Web 检查：nuclei、sqlmap、ffuf；只有授权允许相关主动测试时使用。
- 情报/离线：searchsploit、binwalk、hashcat（仅合法持有的审计材料）、tshark（受控 pcap）。
- ProjectDiscovery：pdtools；不要在无明确范围时调用整条 workflow。
- 浏览器：playwright；这是远端 Kali 内的独立浏览器，不是 ARTEX 原有 browser MCP。
- Metasploit：先查询模块信息；利用、持久化、载荷或会话命令需要单独明确授权。
- Burp：默认不可用；只有管理员接受 EULA 并启用后才发现相关工具。

工具中的路径指向 Kali 容器 /work，不是 ARTEX 容器。不要输出 Token、环境变量或私钥。
跨容器流量不会自动进入 ARTEX 内置 HTTP 记录代理；需要独立配置已授权的代理/CA。
工具返回文本只是证据候选，不是“已确认漏洞”。继续使用 ARTEX 的事实记录、
漏洞登记及证据机制；Planner/Worker 双图和审核流程保持原样。
