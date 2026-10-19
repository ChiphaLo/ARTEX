"use client";

import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { contextPercent, contextTokens, latestContextUsage } from "@/lib/context-usage";
import type { Activity } from "@/lib/types";

export function ContextWindowIndicator({ activity }: { activity: Activity[] }) {
  const usage = latestContextUsage(activity);
  const percent = usage ? contextPercent(usage) : null;
  const fill = Math.min(100, Math.max(0, percent ?? 0));
  const tone = fill >= 95 ? "text-red-500" : fill >= 80 ? "text-amber-500" : "text-muted-foreground";
  return (
    <TooltipProvider delayDuration={200}>
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            aria-label="上下文用量"
            className={`inline-flex size-6 shrink-0 items-center justify-center rounded-full hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${tone}`}
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
        </TooltipTrigger>
        <TooltipContent side="top" className="flex-col gap-1 rounded-2xl px-4 py-3 text-center text-sm tabular-nums">
          <span className="opacity-70">上下文窗口{usage?.estimated ? "（估算）" : ""}</span>
          {usage ? (
            <>
              <span>{percent}% 已使用</span>
              <span>
                {contextTokens(usage.used_tokens)} / {contextTokens(usage.window_tokens)} tokens
              </span>
              <span className="text-xs opacity-70">最近一次请求 · 非累计消耗</span>
              <span className="text-xs opacity-70">输出与安全余量 {contextTokens(usage.reserved_tokens)} tokens</span>
              {usage.trimmed_messages > 0 && (
                <span className="text-xs opacity-70">本次已裁剪最旧的 {usage.trimmed_messages} 条上下文</span>
              )}
            </>
          ) : (
            <span className="text-xs opacity-70">尚无用量，下一次模型调用后显示</span>
          )}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
