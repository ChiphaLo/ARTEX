"use client";
import * as React from "react";
import { ImagePlusIcon, ImageIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import type { ChatAttachment } from "@/lib/types";

export function ImagePicker({ onFiles, disabled }: { onFiles: (files: File[]) => void; disabled?: boolean }) {
  const ref = React.useRef<HTMLInputElement>(null);
  return (
    <>
      <input
        ref={ref}
        type="file"
        accept="image/png,image/jpeg,image/gif,image/webp"
        multiple
        className="hidden"
        onChange={(e) => {
          const files = Array.from(e.target.files ?? []);
          e.target.value = "";
          if (files.length) onFiles(files);
        }}
      />
      <Button
        type="button"
        size="icon"
        variant="ghost"
        disabled={disabled}
        title="添加图片（也可粘贴截图）"
        aria-label="添加图片"
        onClick={() => ref.current?.click()}
      >
        <ImagePlusIcon className="size-4" />
      </Button>
    </>
  );
}
export function ChatImagePreview({ attachment }: { attachment: ChatAttachment }) {
  const [src, setSrc] = React.useState("");
  const url = attachment.preview_url;
  React.useEffect(() => {
    setSrc("");
    if (!url?.startsWith("/api/chat/image?")) return;
    const controller = new AbortController();
    let objectURL = "";
    const token = localStorage.getItem("artex_token");
    void fetch(url, { signal: controller.signal, headers: token ? { Authorization: `Bearer ${token}` } : {} })
      .then(async (r) => {
        if (!r.ok) throw new Error("preview unavailable");
        return r.blob();
      })
      .then((blob) => {
        if (controller.signal.aborted) return;
        objectURL = URL.createObjectURL(blob);
        setSrc(objectURL);
      })
      .catch(() => {});
    return () => {
      controller.abort();
      if (objectURL) URL.revokeObjectURL(objectURL);
    };
  }, [url]);
  if (!attachment.media_type?.startsWith("image/")) return null;
  return src ? (
    <img src={src} alt={attachment.name} className="size-14 rounded object-contain" />
  ) : (
    <ImageIcon className="size-5" aria-label="图片" />
  );
}
