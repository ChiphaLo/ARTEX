"use client";
import { ChatImagePreview, ImagePicker } from "@/components/chat-images";

import * as React from "react";

import { Loader2Icon, PaperclipIcon, PlayIcon, SquareIcon } from "lucide-react";
import { toast } from "sonner";

import { AgentInbox, useInboxRequestID } from "@/components/agent-inbox";
import { MentionTextarea } from "@/components/mention-textarea";
import { Button } from "@/components/ui/button";
import { type AgentMessageMode, type AgentRunStatus, api } from "@/lib/api";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import type { ChatAttachment } from "@/lib/types";

function useAgentRun(base: string) {
  const [status, setStatus] = React.useState<AgentRunStatus>({ running: false, can_continue: false });
  const sequence = React.useRef(0);
  React.useEffect(() => {
    let alive = true,
      fetching = false;
    setStatus({ running: false, can_continue: false });
    const poll = async () => {
      if (fetching) return;
      fetching = true;
      const n = ++sequence.current;
      try {
        const r = await api.agentRunStatus(base);
        if (alive && n === sequence.current) setStatus(r);
      } catch {
        /* Original transcript polling surfaces connectivity errors. */
      } finally {
        fetching = false;
      }
    };
    void poll();
    const timer = setInterval(() => void poll(), 1000);
    return () => {
      alive = false;
      sequence.current++;
      clearInterval(timer);
    };
  }, [base]);
  return {
    status,
    started: () => {
      sequence.current++;
      setStatus((old) => ({ ...old, running: true, can_continue: false }));
    },
  };
}
export function AgentContinueButton({ base, onStarted }: { base: string; onStarted?: () => void }) {
  return <ContinueControl key={base} base={base} onStarted={onStarted} />;
}
function ContinueControl({ base, onStarted }: { base: string; onStarted?: () => void }) {
  const run = useAgentRun(base);
  return (
    <ContinueAction
      base={base}
      status={run.status}
      onStarted={() => {
        run.started();
        onStarted?.();
      }}
    />
  );
}
function ContinueAction({ base, status, onStarted }: { base: string; status: AgentRunStatus; onStarted: () => void }) {
  const [sending, setSending] = React.useState(false);
  const inFlight = React.useRef(false);
  const alive = React.useRef(true);
  React.useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  if (!status.can_continue || !status.run_id) return null;
  async function resume() {
    if (inFlight.current || !status.run_id) return;
    inFlight.current = true;
    setSending(true);
    try {
      await api.continueAgentRun(base, status.run_id);
      if (alive.current) onStarted();
    } catch (e) {
      toast.error(`继续失败：${(e as Error).message}`);
    } finally {
      inFlight.current = false;
      if (alive.current) setSending(false);
    }
  }
  return (
    <Button
      size="sm"
      variant="outline"
      disabled={sending}
      onClick={() => void resume()}
      title="恢复原会话的中断进度，不重放全部工具；不需要输入 continue"
      aria-label="继续中断的会话"
    >
      {sending ? <Loader2Icon className="size-3.5 animate-spin" /> : <PlayIcon className="size-3.5" />}继续
    </Button>
  );
}
export function PlannerComposer({ taskId }: { taskId: string }) {
  return <PlannerInput key={taskId} taskId={taskId} />;
}
function PlannerInput({ taskId }: { taskId: string }) {
  const base = `/tasks/${taskId}/planner`;
  const run = useAgentRun(base);
  const [message, setMessage] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [stopping, setStopping] = React.useState(false);
  const [uploading, setUploading] = React.useState(false);
  const [attachments, setAttachments] = React.useState<ChatAttachment[]>([]);
  const [refresh, setRefresh] = React.useState(0);
  const fileInput = React.useRef<HTMLInputElement>(null);
  const inFlight = React.useRef(false);
  const requestID = useInboxRequestID();
  const sendMode = useChatSendMode();
  async function send(mode: AgentMessageMode) {
    const text = message.trim(),
      atts = attachments;
    if (inFlight.current || uploading || (!text && !atts.length)) return;
    const id = requestID.getID({ taskId, mode, text, attachments: atts });
    inFlight.current = true;
    setSending(true);
    try {
      await api.sendPlannerMessage(taskId, text, mode, id, atts);
      requestID.clear();
      setMessage("");
      setAttachments([]);
      setRefresh((r) => r + 1);
    } catch (e) {
      toast.error(`发送失败：${(e as Error).message}`);
    } finally {
      inFlight.current = false;
      setSending(false);
    }
  }
  async function stop() {
    if (stopping) return;
    setStopping(true);
    try {
      await api.stopPlanner(taskId);
      setRefresh((r) => r + 1);
    } catch (e) {
      toast.error(`停止失败：${(e as Error).message}`);
    } finally {
      setStopping(false);
    }
  }
  async function upload(files: File[]) {
    if (!files.length) return;
    setUploading(true);
    try {
      const r = await api.chatUpload("task", taskId, files);
      setAttachments((a) => [...a, ...r.attachments]);
    } catch (e) {
      toast.error(`上传失败：${(e as Error).message}`);
    } finally {
      setUploading(false);
    }
  }
  const disabled = sending || uploading;
  return (
    <div className="border-t p-3">
      <AgentInbox base={`${base}/inbox`} refreshKey={refresh} />
      <div className="mb-2 flex flex-wrap items-center gap-2 text-muted-foreground text-xs">
        <span>
          {run.status.running
            ? "Planner 运行中，可发送非打断 Steer"
            : "给 Planner 发消息；不会自动创建目标或恢复整个任务"}
        </span>
        <ContinueAction base={base} status={run.status} onStarted={run.started} />
      </div>
      {attachments.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1">
          {attachments.map((a) => (
            <button
              key={a.path}
              type="button"
              disabled={disabled}
              className="rounded border px-2 py-1 text-xs"
              title="移除附件"
              onClick={() => setAttachments((prev) => prev.filter((p) => p.path !== a.path))}
            >
              <ChatImagePreview attachment={a} />
              {a.name} ×
            </button>
          ))}
        </div>
      )}
      <MentionTextarea
        onFiles={disabled ? undefined : upload}
        value={message}
        onValueChange={setMessage}
        disabled={disabled}
        rows={2}
        aria-label="给 Planner 发消息"
        placeholder="输入人工规划要求，@ 引用记录；Steer 不打断当前批次，默认发送为排队"
        onKeyDown={(e) => {
          if (shouldSubmitOnKey(e, sendMode)) {
            e.preventDefault();
            void send("queue");
          }
        }}
      />
      <div className="mt-2 flex items-center gap-2">
        <input
          type="file"
          multiple
          ref={fileInput}
          className="hidden"
          onChange={(e) => {
            const files = Array.from(e.target.files ?? []);
            e.target.value = "";
            void upload(files);
          }}
        />
        <Button
          size="icon"
          variant="ghost"
          title="上传附件"
          disabled={disabled}
          onClick={() => fileInput.current?.click()}
        >
          <PaperclipIcon className="size-4" />
        </Button>
        <ImagePicker onFiles={upload} disabled={disabled} />
        <span className="mr-auto text-muted-foreground text-xs">{[...message].length}</span>
        <Button
          size="sm"
          variant="outline"
          disabled={disabled || (!message.trim() && !attachments.length)}
          onClick={() => void send("steer")}
        >
          Steer
        </Button>
        <Button
          size="sm"
          disabled={disabled || (!message.trim() && !attachments.length)}
          onClick={() => void send("queue")}
        >
          {sending ? <Loader2Icon className="size-4 animate-spin" /> : "排队发送"}
        </Button>
        {run.status.running && (
          <Button
            size="sm"
            variant="destructive"
            disabled={stopping}
            onClick={() => void stop()}
            title="只停止 Planner 当前轮，不停止 Worker 或清空人工队列"
          >
            <SquareIcon className="size-3.5" />
            停止
          </Button>
        )}
      </div>
    </div>
  );
}
