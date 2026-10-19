"use client";

import * as React from "react";

import { ArrowDownIcon, ArrowUpIcon, GripVerticalIcon, PencilIcon, Trash2Icon, SendIcon } from "lucide-react";
import { toast } from "sonner";

import { MentionTextarea } from "@/components/mention-textarea";
import { Button } from "@/components/ui/button";
import { type AgentInboxItem, api } from "@/lib/api";
import { newRequestID } from "@/lib/request-id";
import type { ChatAttachment } from "@/lib/types";

// The same payload keeps its ID after an unknown HTTP outcome. Editing the
// draft/mode/scope creates a new operation, not a retry of the previous message.
export function useInboxRequestID() {
  const last = React.useRef({ payload: "", id: "" });
  return {
    getID: (payload: unknown) => {
      const key = JSON.stringify(payload);
      if (!last.current.id || last.current.payload !== key) last.current = { payload: key, id: newRequestID() };
      return last.current.id;
    },
    clear: () => {
      last.current = { payload: "", id: "" };
    },
  };
}

export function AgentInbox({ base, refreshKey = 0 }: { base: string; refreshKey?: number }) {
  // Keyed inner component prevents late requests/drafts from crossing scopes.
  return <InboxList key={base} base={base} refreshKey={refreshKey} />;
}
function InboxList({ base, refreshKey }: { base: string; refreshKey: number }) {
  const [items, setItems] = React.useState<AgentInboxItem[]>([]);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [editing, setEditing] = React.useState<AgentInboxItem | null>(null);
  const [text, setText] = React.useState("");
  const [attachments, setAttachments] = React.useState<ChatAttachment[]>([]);
  const [dragID, setDragID] = React.useState<number | null>(null);
  const alive = React.useRef(true);
  const sequence = React.useRef(0);
  const mutating = React.useRef(false);
  const dragOrder = React.useRef<number[]>([]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: Successful enqueue explicitly refreshes this scope.
  React.useEffect(() => {
    alive.current = true;
    let active = true;
    let fetching = false;
    const refresh = async () => {
      if (fetching || mutating.current) return;
      fetching = true;
      const n = ++sequence.current;
      try {
        const r = await api.agentInbox(base);
        if (active && n === sequence.current) {
          setItems(r.items ?? []);
          setError("");
        }
      } catch (e) {
        if (active && n === sequence.current) setError((e as Error).message);
      } finally {
        fetching = false;
      }
    };
    void refresh();
    const timer = setInterval(() => void refresh(), 1500);
    return () => {
      active = false;
      alive.current = false;
      clearInterval(timer);
      sequence.current++;
    };
  }, [base, refreshKey]);
  async function mutate(fn: () => Promise<{ items: AgentInboxItem[] }>) {
    if (mutating.current) return;
    mutating.current = true;
    sequence.current++;
    setBusy(true);
    try {
      const r = await fn();
      if (alive.current) {
        setItems(r.items ?? []);
        setEditing(null);
        setError("");
      }
    } catch (e) {
      if (alive.current) {
        toast.error((e as Error).message);
        // Fetch actual state after conflict; keep the unsaved draft visible.
        try {
          const r = await api.agentInbox(base);
          if (alive.current) setItems(r.items ?? []);
        } catch {
          /* next poll retries */
        }
      }
    } finally {
      mutating.current = false;
      if (alive.current) setBusy(false);
    }
  }
  const queued = items.filter((i) => i.mode === "queue" && i.state === "pending");
  const ids = queued.map((i) => i.id);
  function move(id: number, target: number, previous = ids) {
    const from = previous.indexOf(id),
      to = previous.indexOf(target);
    if (from < 0 || to < 0 || from === to) return;
    const next = [...previous];
    next.splice(from, 1);
    next.splice(to, 0, id);
    void mutate(() => api.reorderAgentInbox(base, next, previous));
  }
  if (queued.length === 0 && !error && !editing) return null;
  return (
    <section
      className="max-h-64 shrink-0 overflow-y-auto border-t bg-muted/20 px-3 py-2 text-xs"
      aria-label="Agent 待处理消息"
    >
      <div className="mb-1 font-medium">排队消息 ({queued.length})</div>
      {error && (
        <p className="text-destructive" role="alert">
          队列读取失败：{error}
        </p>
      )}
      {queued.map((item) => {
        const pending = item.state === "pending";
        const sortable = pending && item.mode === "queue";
        const index = ids.indexOf(item.id);
        let label = `排队 ${index + 1}`;
        if (item.mode === "steer") label = "Steer 待接收";
        if (!pending) label = "处理中";
        return (
          <fieldset
            key={item.id}
            aria-label={label}
            className={`mb-1 rounded border bg-background p-2 ${dragID === item.id ? "opacity-50" : ""}`}
            onDragOver={(e) => {
              if (sortable && dragID !== null) e.preventDefault();
            }}
            onDrop={(e) => {
              e.preventDefault();
              if (!busy && sortable && dragID !== null) move(dragID, item.id, dragOrder.current);
              setDragID(null);
            }}
          >
            <div className="flex items-start gap-1.5">
              {sortable && (
                <button
                  type="button"
                  draggable={!busy}
                  disabled={busy}
                  aria-label="拖动排队消息"
                  title="拖动改变顺序"
                  className="cursor-grab touch-none"
                  onDragStart={(e) => {
                    setDragID(item.id);
                    dragOrder.current = ids;
                    e.dataTransfer.effectAllowed = "move";
                    e.dataTransfer.setData("text/plain", String(item.id));
                  }}
                  onDragEnd={() => setDragID(null)}
                >
                  <GripVerticalIcon className="size-3.5" />
                </button>
              )}
              <span className="shrink-0 text-muted-foreground">{label}</span>
              <span className="line-clamp-3 min-w-0 flex-1 whitespace-pre-wrap break-words">
                {item.message || "附件消息"}
              </span>
              {sortable && (
                <>
                  <button
                    type="button"
                    disabled={busy}
                    title="立即转为 Steer（不打断当前工具）"
                    aria-label="排队消息转为 Steer"
                    onClick={() =>
                      void mutate(() =>
                        api.editAgentInbox(base, item.id, item.message, item.revision, item.attachments, "steer"),
                      )
                    }
                  >
                    <SendIcon className="size-3.5" />
                  </button>
                  <button
                    type="button"
                    disabled={busy || index === 0}
                    title="上移"
                    aria-label="排队消息上移"
                    onClick={() => move(item.id, ids[index - 1])}
                  >
                    <ArrowUpIcon className="size-3.5" />
                  </button>
                  <button
                    type="button"
                    disabled={busy || index === ids.length - 1}
                    title="下移"
                    aria-label="排队消息下移"
                    onClick={() => move(item.id, ids[index + 1])}
                  >
                    <ArrowDownIcon className="size-3.5" />
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    title="修改排队消息"
                    aria-label="修改排队消息"
                    onClick={() => {
                      setEditing(item);
                      setText(item.message);
                      setAttachments(item.attachments ?? []);
                    }}
                  >
                    <PencilIcon className="size-3.5" />
                  </button>
                </>
              )}
              {pending && (
                <button
                  type="button"
                  disabled={busy}
                  title="删除待处理消息"
                  aria-label="删除待处理消息"
                  onClick={() => void mutate(() => api.deleteAgentInbox(base, item.id))}
                >
                  <Trash2Icon className="size-3.5" />
                </button>
              )}
            </div>
            {item.attachments?.length ? (
              <p className="mt-1 truncate text-muted-foreground">
                附件：{item.attachments.map((a) => a.name).join("、")}
              </p>
            ) : null}
            {editing?.id === item.id && (
              <div className="mt-2">
                <MentionTextarea
                  value={text}
                  onValueChange={setText}
                  disabled={busy}
                  aria-label="修改排队消息内容"
                  rows={2}
                />
                {attachments.map((a) => (
                  <button
                    key={a.path}
                    type="button"
                    disabled={busy}
                    className="m-1 rounded border px-1"
                    title="移除附件"
                    onClick={() => setAttachments((prev) => prev.filter((x) => x.path !== a.path))}
                  >
                    {a.name} ×
                  </button>
                ))}
                <div className="mt-1 flex gap-2">
                  <Button
                    size="sm"
                    disabled={busy || !sortable || (!text.trim() && !attachments.length) || [...text].length > 4000}
                    onClick={() =>
                      void mutate(() => api.editAgentInbox(base, item.id, text, editing.revision, attachments))
                    }
                  >
                    保存
                  </Button>
                  <Button size="sm" variant="ghost" disabled={busy} onClick={() => setEditing(null)}>
                    取消修改
                  </Button>
                </div>
              </div>
            )}
          </fieldset>
        );
      })}
      {editing && !items.some((i) => i.id === editing.id) && (
        <div role="alert" className="text-destructive">
          这条消息已开始处理或移除，未保存的修改不会覆盖正在运行的消息。<p className="whitespace-pre-wrap">{text}</p>
          <Button size="sm" variant="ghost" onClick={() => setEditing(null)}>
            关闭
          </Button>
        </div>
      )}
    </section>
  );
}
