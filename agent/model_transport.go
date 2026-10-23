package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
)

const modelHeaderTimeout = 120 * time.Second
const modelFirstByteTimeout = 120 * time.Second
const modelIdleTimeout = 90 * time.Second

type modelWaitTimeout struct {
	phase    string
	duration time.Duration
}

func (e *modelWaitTimeout) Error() string {
	return fmt.Sprintf("模型上游等待超时：%s，持续 %s 未收到数据", e.phase, e.duration)
}
func (e *modelWaitTimeout) Timeout() bool   { return true }
func (e *modelWaitTimeout) Temporary() bool { return true }

// Timeout the individual HTTP attempt, never the session context. Incoming data
// resets the idle timer; a long but active reasoning stream has no total cap.
func observedModelRoundTrip(req *http.Request, base http.RoundTripper, header, first, idle time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	b := &modelResponseBody{ctx: ctx, cancel: cancel, started: time.Now(), idle: idle}
	b.arm(header, "等待连接或 HTTP 响应头")
	llm.ReportCallProgress(ctx, b.progress("connecting"))
	resp, err := base.RoundTrip(req.Clone(ctx))
	if err != nil {
		mapped := b.translate(err)
		b.finish()
		b.cancel(nil)
		return resp, mapped
	}
	if resp == nil {
		b.finish()
		b.cancel(nil)
		return nil, fmt.Errorf("模型上游未返回 HTTP 响应")
	}
	b.arm(first, "等待首个响应字节")
	llm.ReportCallProgress(ctx, b.progress("waiting_first_byte"))
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	b.body = resp.Body
	resp.Body = b
	return resp, nil
}

type modelResponseBody struct {
	body       io.ReadCloser
	ctx        context.Context
	cancel     context.CancelCauseFunc
	mu         sync.Mutex
	timer      *time.Timer
	generation uint64
	closed     bool
	started    time.Time
	last       time.Time
	bytes      int64
	idle       time.Duration
}

func (b *modelResponseBody) arm(d time.Duration, phase string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.generation++
	g := b.generation
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(d, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.closed && g == b.generation {
			b.cancel(&modelWaitTimeout{phase: phase, duration: d})
		}
	})
}
func (b *modelResponseBody) progress(phase string) llm.CallProgress {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := llm.CallProgress{Phase: phase, StartedAt: b.started.UnixMilli(), BytesReceived: b.bytes}
	if !b.last.IsZero() {
		p.LastDataAt = b.last.UnixMilli()
	}
	return p
}
func (b *modelResponseBody) finish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.generation++
	if b.timer != nil {
		b.timer.Stop()
	}
}
func (b *modelResponseBody) translate(err error) error {
	var timeout *modelWaitTimeout
	if errors.As(context.Cause(b.ctx), &timeout) {
		return timeout
	}
	return err
}
func (b *modelResponseBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.bytes += int64(n)
		b.last = time.Now()
		b.mu.Unlock()
		b.arm(b.idle, "等待后续流式数据")
		llm.ReportCallProgress(b.ctx, b.progress("receiving"))
	}
	if err != nil {
		b.finish()
		err = b.translate(err)
	}
	return n, err
}
func (b *modelResponseBody) Close() error {
	b.finish()
	llm.ReportCallProgress(b.ctx, b.progress("request_finished"))
	b.cancel(nil)
	return b.body.Close()
}
