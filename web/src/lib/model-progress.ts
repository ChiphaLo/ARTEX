import type { Activity } from "./types";
// Pure formatter shared by live UI tests. Transport updates are status, never replies.
export function modelProgressLabel(activity: Activity[], now: number): string {
  for (let i = activity.length - 1; i >= 0; i--) {
    const row = activity[i];
    if (["tool_use", "tool_result", "result", "user", "round", "retry"].includes(row.kind)) {
      if (row.kind === "tool_use") return `正在执行工具：${row.tool ?? "工具"}`;
      if (row.kind === "retry") return row.summary;
      return "处理中…";
    }
    if (row.kind !== "model_status" || !row.metadata?.model_progress) continue;
    const p = row.metadata.model_progress;
    const prefix =
      p.stage === "compacting"
        ? `正在压缩上下文 · 第 ${p.chunk ?? 1} 批（${p.groups_done ?? 0}/${p.total_groups ?? 0} 组） · `
        : "";
    const elapsed = p.started_at ? ` · 已等待 ${Math.max(0, Math.floor((now - p.started_at) / 1000))}s` : "";
    switch (p.phase) {
      case "rate_limit":
        return `${prefix}等待本地限流配额…`;
      case "starting":
        return `${prefix}准备请求…`;
      case "connecting":
        // The server bounds header waits at 120s. A much older last progress
        // event can be stale after DB/SSE failure; never assert it is still a
        // live upstream request merely because the timestamp keeps aging.
        if (p.started_at && now - p.started_at > 130_000) {
          return `${prefix}连接进度已超过预期，状态可能已失联；请检查会话状态`;
        }
        return `${prefix}连接上游／等待响应头${elapsed}`;
      case "waiting_first_byte":
        return `${prefix}已收到响应头，等待首字节${elapsed}`;
      case "receiving": {
        const silence = p.last_data_at ? Math.max(0, Math.floor((now - p.last_data_at) / 1000)) : 0;
        return `${prefix}已接收 ${((p.bytes_received ?? 0) / 1024).toFixed(1)} KiB · ${silence}s 前收到数据`;
      }
      case "chunk_finished":
        return `${prefix}本批摘要完成`;
      case "request_finished":
        return `${prefix}本次请求结束，处理中…`;
    }
  }
  return "准备模型请求…";
}
