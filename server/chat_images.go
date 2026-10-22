package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
)

// No character cap; retain transport limits for memory/DoS protection.
const maxChatMessageBytes = 8 << 20
const maxImageBytes = 10 << 20
const maxTurnImageBytes = 20 << 20

func supportedChatImage(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}
func validAttachmentPath(path string) bool {
	name := strings.TrimPrefix(path, "uploads/")
	return strings.HasPrefix(path, "uploads/") && len(path) < 1024 && filepath.Base(path) == name && !strings.ContainsAny(name, `/\:`) && name != "" && name != "." && name != ".."
}

// os.Root rejects traversal and symlink escapes. Never trust client Abs/MIME/Size.
func openChatAttachment(baseDir, path string) (*os.File, error) {
	if !validAttachmentPath(path) {
		return nil, fmt.Errorf("非法附件路径")
	}
	root, err := os.OpenRoot(baseDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("附件不是普通文件")
	}
	return f, nil
}
func chatImageBlocks(baseDir string, atts []chatAttachment) ([]llm.ContentBlock, error) {
	if len(atts) > 32 {
		return nil, fmt.Errorf("附件最多 32 个")
	}
	var out []llm.ContentBlock
	total := 0
	for _, a := range atts {
		f, err := openChatAttachment(baseDir, a.Path)
		if err != nil {
			return nil, fmt.Errorf("读取附件 %s 失败: %w", a.Name, err)
		}
		head := make([]byte, 512)
		n, _ := f.Read(head)
		mime := http.DetectContentType(head[:n])
		if !supportedChatImage(mime) {
			f.Close()
			continue
		}
		_, err = f.Seek(0, io.SeekStart)
		if err != nil {
			f.Close()
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		total += len(data)
		if len(data) > maxImageBytes || total > maxTurnImageBytes {
			return nil, fmt.Errorf("图片单张最多 10 MiB，每条消息图片合计最多 20 MiB")
		}
		out = append(out, llm.ContentBlock{Type: llm.BlockImage, Source: &llm.ImageSource{Type: "base64", MediaType: mime, Data: base64.StdEncoding.EncodeToString(data)}})
	}
	return out, nil
}
func withChatImages(ctx context.Context, baseDir string, atts []chatAttachment) context.Context {
	return agent.WithInputContent(ctx, func() ([]llm.ContentBlock, error) { return chatImageBlocks(baseDir, atts) })
}

// Authenticated preview: no arbitrary paths/URLs, SVG/HTML or tokens in URLs.
func (s *Server) chatImage(w http.ResponseWriter, r *http.Request) {
	sub := ""
	switch r.URL.Query().Get("scope") {
	case "task":
		sub = "tasks"
	case "session":
		sub = "sessions"
	case "staging":
		sub = "drafts"
	default:
		writeErr(w, 400, "invalid scope")
		return
	}
	id := r.URL.Query().Get("id")
	if !safeChatID.MatchString(id) {
		writeErr(w, 400, "invalid id")
		return
	}
	f, err := openChatAttachment(filepath.Join(s.m.dir, sub, id), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 404, "image not found")
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || stat.Size() > maxImageBytes {
		writeErr(w, 400, "image too large")
		return
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	mime := http.DetectContentType(head[:n])
	if !supportedChatImage(mime) {
		writeErr(w, 415, "unsupported image")
		return
	}
	f.Seek(0, io.SeekStart)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
}
