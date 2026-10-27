"use client";

import * as React from "react";
import { XIcon } from "lucide-react";

import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { contextBudgetTokens, contextPercent, contextTokens, latestContextUsage } from "@/lib/context-usage";
import type { Activity } from "@/lib/types";

export function ContextWindowIndicator({ activity }: { activity: Activity[] }) {
  const [open, setOpen] = React.useState(false);
  const usage = latestContextUsage(activity);
  const percent = usage ? contextPercent(usage) : null;
  const fill = Math.min(100, Math.max(0, percent ?? 0));
  let tone = "text-muted-foreground";
  if (fill >= 95) tone = "text-red-500";
  else if (fill >= 90) tone = "text-amber-500";
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label="上下文用量"
          title="点击查看上下文用量"
          className={`inline-flex size-8 shrink-0 touch-manipulation items-center justify-center rounded-full hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring [@media(pointer:coarse)]:size-10 ${tone}`}
        >
          <svg viewBox="0 0 20 20" className="size-4" aria-hidden="true">
            <circle cx="10" cy="10" r="7" fill="none" stroke="currentColor" strokeWidth="3" opacity="0.2" />
            <circle
              cx="10"
              cy="10"
              r="7"
              fill="none"
              stroke="currentColor"
              strokeWidth="3"
              pathLength="100"
              strokeDasharray={`${fill} 100`}
              strokeLinecap="round"
              transform="rotate(-90 10 10)"
            />
          </svg>
        </button>
      </PopoverTrigger>
      <PopoverContent side="top" collisionPadding={8} aria-label="上下文窗口详情" className="max-h-[var(--radix-popover-content-available-height)] w-[min(20rem,calc(100vw-2rem))] gap-1 overflow-y-auto rounded-2xl px-4 py-3 text-center text-sm tabular-nums">
        <div className="flex items-center justify-between gap-2">
          <span className="text-muted-foreground">上下文窗口{usage?.estimated ? "（估算）" : ""}</span>
          <button type="button" aria-label="关闭上下文详情" onClick={() => setOpen(false)} className="inline-flex size-8 shrink-0 items-center justify-center rounded-md hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring [@media(pointer:coarse)]:size-10">
            <XIcon className="size-4" aria-hidden="true" />
          </button>
        </div>
        {usage ? (
          <>
            <span>{percent}% 请求预算占用</span>
            <span>
              {contextTokens(contextBudgetTokens(usage))} / {contextTokens(usage.window_tokens)} tokens
            </span>
            <span className="text-xs opacity-70">
              {usage.phase === "before_compaction" ? "压缩前上下文 · 非累计消耗" : "最近一次请求 · 非累计消耗"}
            </span>
            <span className="text-xs opacity-70">输入估算 {contextTokens(usage.used_tokens)} tokens</span>
            <span className="text-xs opacity-70">输出与安全余量 {contextTokens(usage.reserved_tokens)} tokens</span>
            <span className="text-xs opacity-70">90% 尝试自动压缩 · 超过 100% 裁剪兜底</span>
            {usage.trimmed_messages > 0 && (
              <span className="text-xs opacity-70">本次已裁剪最旧的 {usage.trimmed_messages} 条上下文</span>
            )}
          </>
        ) : (
          <span className="text-xs opacity-70">尚无用量，下一次模型调用后显示</span>
        )}
      </PopoverContent>
    </Popover>
  );
}
