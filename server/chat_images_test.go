package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func chatPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestChatImagesUploadPreviewAndModelContent(t *testing.T) {
	dir := t.TempDir()
	s := &Server{m: &Manager{dir: dir}}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("file", "screenshot.png")
	data := chatPNG(t)
	part.Write(data)
	form.Close()
	r := httptest.NewRequest("POST", "/api/chat/upload?scope=session&id=conv-1", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	s.chatUpload(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var uploaded struct {
		Attachments []chatAttachment `json:"attachments"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if len(uploaded.Attachments) != 1 {
		t.Fatal(uploaded)
	}
	a := uploaded.Attachments[0]
	if a.MediaType != "image/png" || a.PreviewURL == "" {
		t.Fatal(a)
	}
	w = httptest.NewRecorder()
	s.chatImage(w, httptest.NewRequest("GET", a.PreviewURL, nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("preview mismatch", w.Code)
	}
	a.Abs = "C:/do-not-read"
	a.Size = 0
	a.MediaType = "image/jpeg"
	blocks, err := chatImageBlocks(filepath.Join(dir, "sessions", "conv-1"), []chatAttachment{a})
	if err != nil || len(blocks) != 1 || blocks[0].Type != llm.BlockImage || blocks[0].Source.MediaType != "image/png" {
		t.Fatal(blocks, err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(blocks[0].Source.Data)
	if !bytes.Equal(decoded, data) {
		t.Fatal("pixels changed")
	}
}
func TestChatImagesRejectTraversalAndOversize(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "uploads"), 0700)
	for _, path := range []string{"../secret", "uploads/../secret", "uploads/x/../../secret", "C:/secret", "uploads/x:y", "uploads/..", "uploads/", `uploads/..\secret`} {
		if _, err := openChatAttachment(dir, path); err == nil {
			t.Fatal("accepted", path)
		}
	}
	oversized := append(chatPNG(t), make([]byte, maxImageBytes)...)
	os.WriteFile(filepath.Join(dir, "uploads", "large.png"), oversized, 0600)
	if _, err := chatImageBlocks(dir, []chatAttachment{{Path: "uploads/large.png"}}); err == nil {
		t.Fatal("accepted oversized image")
	}
	os.WriteFile(filepath.Join(dir, "uploads", "text.txt"), []byte("plain text"), 0600)
	blocks, err := chatImageBlocks(dir, []chatAttachment{{Path: "uploads/text.txt", MediaType: "image/png"}})
	if err != nil || len(blocks) != 0 {
		t.Fatal("trusted claimed mime", err)
	}
	outside := filepath.Join(t.TempDir(), "secret.png")
	os.WriteFile(outside, chatPNG(t), 0600)
	if err := os.Symlink(outside, filepath.Join(dir, "uploads", "link.png")); err == nil {
		if _, err := chatImageBlocks(dir, []chatAttachment{{Path: "uploads/link.png"}}); err == nil {
			t.Fatal("symlink escaped")
		}
	}
}
func TestChatLongInputNoCharacterCap(t *testing.T) {
	for _, msg := range []string{strings.Repeat("中文", 3000), strings.Repeat("x", 128<<10)} {
		w := httptest.NewRecorder()
		if !validInboxMessage(w, msg, nil) {
			t.Fatal(w.Body.String())
		}
		body, _ := json.Marshal(map[string]string{"message": msg})
		r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
		var decoded map[string]string
		if !decodeConversationRequest(w, r, &decoded) || decoded["message"] != msg {
			t.Fatal("long text rejected")
		}
	}
	w := httptest.NewRecorder()
	if validInboxMessage(w, "", nil) {
		t.Fatal("empty accepted")
	}
	w = httptest.NewRecorder()
	if !validInboxMessage(w, "", []chatAttachment{{Path: "uploads/image.png"}}) {
		t.Fatal("image-only rejected")
	}
}

// Exercise real HTTP admission, inbox persistence/drain, SDK prompt and provider
// calls, against the isolated test database and a synthetic model (no network LLM).
func TestChatImagesInboxDelivery(t *testing.T) {
	for _, conv := range []bool{false, true} {
		t.Run(fmt.Sprint(conv), func(t *testing.T) {
			s, task, p := inboxFixture(t)
			tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
			s.taskAgents[task.ID].main = agent.NewMainAgent(p, "fixture", s.m.dir, tx, 1<<20, 12)
			s.chatAgent = agent.NewChatAgent(p, "fixture", s.m.dir, tx, 1<<20)
			convID := int64(0)
			baseDir := filepath.Join(s.m.dir, "tasks", task.ID)
			scope := mainInboxScope(task.ID, 0)
			if conv {
				c, err := s.m.pg.CreateConversation("mainagent", "image fixture", nil)
				if err != nil {
					t.Fatal(err)
				}
				convID = c.ID
				scope = s.convBusyKey(convID)
				baseDir = filepath.Join(s.m.dir, "sessions", scope)
				t.Cleanup(func() { s.cancelConversation(convID); s.m.pg.DeleteConversation(convID) })
			}
			os.MkdirAll(filepath.Join(baseDir, "uploads"), 0700)
			os.WriteFile(filepath.Join(baseDir, "uploads", "pic.png"), chatPNG(t), 0600)
			atts := []chatAttachment{{Name: "pic.png", Path: "uploads/pic.png"}}
			if w := inboxRequest(t, s, task, convID, map[string]any{"message": strings.Repeat("长文本", 2000), "attachments": atts}); w.Code != 202 {
				t.Fatal(w.Code, w.Body.String())
			}
			select {
			case <-p.started:
			case <-time.After(5 * time.Second):
				t.Fatal("direct image did not reach provider")
			}
			for _, mode := range []string{"steer", "queue"} {
				if w := inboxRequest(t, s, task, convID, map[string]any{"message": "", "mode": mode, "request_id": "image-" + mode, "attachments": atts}); w.Code != 202 {
					t.Fatal(mode, w.Code, w.Body.String())
				}
			}
			go s.runAgentInbox()
			close(p.release)
			mainBatchEventually(t, func() bool { items, _ := s.m.pg.AgentInbox(scope); return len(items) == 0 })
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.imageCounts) != 3 {
				t.Fatal("missing deliveries", p.imageCounts)
			}
			for i, n := range p.imageCounts {
				if n != i+1 {
					t.Fatal("image lost/duplicated across direct, steer, queue", p.imageCounts)
				}
			}
		})
	}
}

func TestChatImagesPlannerDelivery(t *testing.T) {
	s, task, p := interactiveFixture(t)
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	s.taskAgents[task.ID].pl = agent.NewPlanner(p, "fixture", s.m.dir, tx, 1<<20, 12)
	dir := filepath.Join(s.m.dir, "tasks", task.ID, "uploads")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "pic.png"), chatPNG(t), 0600)
	atts := []chatAttachment{{Name: "pic.png", Path: "uploads/pic.png"}}
	w := interactiveRequest(s, task, 0, "planner", "messages", map[string]any{"message": strings.Repeat("p", 5000), "attachments": atts})
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("planner not started")
	}
	for _, mode := range []string{"steer", "queue"} {
		w = interactiveRequest(s, task, 0, "planner", "messages", map[string]any{"message": "", "mode": mode, "request_id": "image-" + mode, "attachments": atts})
		if w.Code != 202 {
			t.Fatal(mode, w.Code, w.Body.String())
		}
	}
	close(p.release)
	go s.runAgentInbox()
	scope := plannerScope(task.ID)
	mainBatchEventually(t, func() bool { items, _ := s.m.pg.AgentInbox(scope); return len(items) == 0 })
	waitInteractiveRun(t, s, scope, "completed")
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) != 3 {
		t.Fatal("planner requests", len(p.requests))
	}
	for i, req := range p.requests {
		n := 0
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == llm.BlockImage {
					n++
				}
			}
		}
		if n != i+1 {
			t.Fatalf("planner request %d images=%d", i, n)
		}
	}
}
func TestChatImagesWorkerCachedContext(t *testing.T) {
	s, task, p := interactiveFixture(t)
	worker := agent.NewWorker(p, "fixture", s.m.dir, transcript.NewStore(filepath.Join(s.m.dir, "transcripts")), 1<<20, 12)
	s.engine.SetAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return s.taskAgents[task.ID].pl, worker })
	// Ensure the engine already has a cached cancellation context before upload.
	s.engine.execContextFor(s.ctx, task.ID)
	iid, err := task.Store.AddIntent(map[string]any{"summary": "synthetic image fixture"}, 5, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetIntentState(iid, "paused"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.m.dir, "tasks", task.ID, "uploads")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "pic.png"), chatPNG(t), 0600)
	body, _ := json.Marshal(map[string]any{"message": "", "request_id": "image-worker", "attachments": []chatAttachment{{Name: "pic.png", Path: "uploads/pic.png"}}})
	r := httptest.NewRequest("POST", "/messages", bytes.NewReader(body))
	r.SetPathValue("id", task.ID)
	r.SetPathValue("iid", fmt.Sprint(iid))
	w := httptest.NewRecorder()
	s.sendWorkerMessage(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not started")
	}
	p.mu.Lock()
	n := 0
	for _, m := range p.requests[0].Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockImage {
				n++
			}
		}
	}
	p.mu.Unlock()
	s.engine.StopTask(task.ID)
	if n != 1 {
		t.Fatal("worker image lost through cached context", n)
	}
}
