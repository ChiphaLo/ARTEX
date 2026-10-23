"use client";

import * as React from "react";
import { textPage, TEXT_PAGE_SIZE } from "@/lib/text-page";

// A bounded text node is much cheaper to wrap/layout than a multi-megabyte <pre>.
// Thinking follows the newest page; tool details initially show the command.
// Manually reading older pages disables following, without losing any text.
export const PagedText = React.memo(function PagedText({ text, followTail = false }: { text: string; followTail?: boolean }) {
  const [selected, setSelected] = React.useState<number | "tail" | null>(null);
  const following = selected === "tail" || (selected === null && followTail);
  const page = textPage(text, following ? Math.max(0, Math.ceil(text.length / TEXT_PAGE_SIZE) - 1) : selected ?? 0);
  const preRef = React.useRef<HTMLPreElement>(null);
  // biome-ignore lint/correctness/useExhaustiveDependencies: changed page text requires re-pinning after layout, even when following stays true.
  React.useLayoutEffect(() => {
    if (following && preRef.current) preRef.current.scrollTop = preRef.current.scrollHeight;
  }, [following, page.text]);
  const pauseFollowing = () => {
    const pre = preRef.current;
    if (following && pre && pre.scrollHeight > pre.clientHeight) setSelected(page.index);
  };
  const buttonClass = "rounded px-2 py-1 hover:bg-muted disabled:opacity-40 disabled:cursor-default";
  return (
    <div className="mb-1 ml-7 min-w-0">
      {(page.count > 1 || (followTail && !following)) && (
        <nav className="flex flex-wrap items-center gap-1 text-[11px] text-muted-foreground" aria-label="长内容分段">
          <button type="button" className={buttonClass} disabled={page.index === 0} onClick={() => setSelected(0)}>开头</button>
          <button type="button" className={buttonClass} disabled={page.index === 0} onClick={() => setSelected(page.index - 1)}>上一段</button>
          <span className="tabular-nums">{page.index + 1} / {page.count}</span>
          <button type="button" className={buttonClass} disabled={page.index === page.count - 1} onClick={() => setSelected(page.index + 1)}>下一段</button>
          <button type="button" className={buttonClass} disabled={following} onClick={() => setSelected("tail")}>跟随末尾</button>
        </nav>
      )}
      <pre ref={preRef} onWheel={pauseFollowing} onTouchMove={pauseFollowing}
        onKeyDown={(event) => { if (["ArrowUp", "PageUp", "Home"].includes(event.key)) pauseFollowing(); }}
        className="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded bg-muted/50 p-2 font-mono text-[11px] leading-relaxed">{page.text}</pre>
    </div>
  );
});
