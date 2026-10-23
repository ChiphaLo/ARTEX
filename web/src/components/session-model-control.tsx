"use client";

import { ChevronDownIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { TaskLLMResolution } from "@/lib/types";

export function SessionModelControl({ resolution, onEdit }: {
  resolution: TaskLLMResolution;
  onEdit?: () => void;
}) {
  const provider = resolution.name || resolution.model || "未命名配置";
  const label = resolution.available ? (resolution.model || provider) : "模型不可用";
  const description = resolution.available
    ? [provider, resolution.model].filter(Boolean).join(" / ")
    : resolution.reason || "没有可用的 LLM 配置";
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        {onEdit ? (
          <button
            type="button"
            aria-label={`切换任务模型：${label}`}
            onClick={onEdit}
            className="inline-flex h-8 max-w-48 shrink-0 items-center gap-1 rounded-md border px-2 text-xs hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring [@media(pointer:coarse)]:min-h-10"
          >
            <span className="truncate">{label}</span>
            <ChevronDownIcon className="size-3 shrink-0" aria-hidden="true" />
          </button>
        ) : (
          <Badge variant="outline" className="max-w-48 shrink-0 font-normal" aria-label={`当前模型：${label}`}>
            <span className="truncate">{label}</span>
          </Badge>
        )}
      </TooltipTrigger>
      <TooltipContent side="bottom" className="max-w-xs [overflow-wrap:anywhere]">
        <span>{description}</span>
        {resolution.source === "agent_binding" && (
          <span className="block">此角色已在 Agent 设置中固定配置，优先于任务选择。</span>
        )}
      </TooltipContent>
    </Tooltip>
  );
}
