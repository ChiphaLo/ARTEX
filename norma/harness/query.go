package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// QueryInput is a fully-resolved request to run the loop.
type QueryInput struct {
	ContextFilter   func([]llm.Message) ([]llm.Message, error)
	Provider        llm.Provider
	System          []string
	DynamicBoundary int
	Messages        []llm.Message // conversation including the latest user turn
	// SystemReminder, when non-empty, is prepended as the first message on each
	// model call. It is not stored in conversation history — the host builds it
	// fresh per Prompt call so it never accumulates across turns.
	SystemReminder string
	Tools          *tool.Registry
	// UnavailableToolReason is host-authored context for missing-tool errors;
	// it is diagnostic only and does not change registry or permissions.
	UnavailableToolReason string
	// DeferredTools names tools whose schemas are withheld from the model's tool
	// list (they stay in Tools and are invoked via ExecuteExtraTool). Empty = all
	// tool schemas sent as usual.
	DeferredTools []string
	MaxTokens     int
	Temperature   *float64
	// MaxTurns bounds tool-turns (0 = off). MaxDuration bounds wall-clock time,
	// checked at the turn boundary so a turn is never aborted mid-stream — unlike
	// cancelling ctx, which cuts an in-flight stream/tool (0 = off).
	MaxTurns       int
	MaxDuration    time.Duration
	MaxConcurrency int
	// NonStreaming makes each model call a single non-streaming request (via
	// QueryDeps.CallModelSync / Provider.Complete) instead of a token stream. The
	// loop still emits the same KindText/KindThinking/KindToolUse/KindUsage events
	// to the host — they are surfaced once, when the full response arrives, rather
	// than incrementally. Zero value = streaming (unchanged default).
	NonStreaming bool
	WorkingDir   string
	AgentID      string
	// ToolOutputDir, when set, makes oversized tool output spill to a file there
	// (full content kept) instead of being discarded; MaxToolOutputChars sets the
	// per-tool output budget (head size; 0 = default 30000). See tool.Capture.
	ToolOutputDir      string
	MaxToolOutputChars int

	// Tasks, when non-nil, is the background-task manager shared with the tools.
	// The loop drains its completion notifications into the conversation between
	// tool turns so the model learns when background work finishes.
	Tasks *tool.Manager

	// Todos, when non-nil, enables todo system-reminders: when the model hasn't
	// used TodoWrite for TodoReminderTurnsSinceWrite turns (and it's been at least
	// TodoReminderTurnsBetweenReminder turns since the last reminder), the current
	// list is re-surfaced as a <system-reminder> user message. Throttle turns are
	// derived by scanning the API-visible history, so it survives compaction and
	// resume. 0 thresholds default to 10/10 (mirrors claude-code).
	Todos                            *tool.TodoStore
	TodoReminderTurnsSinceWrite      int
	TodoReminderTurnsBetweenReminder int

	// BashEnv holds extra "KEY=VALUE" entries injected into Bash subprocess
	// environments (foreground + background). Empty = inherit unchanged.
	BashEnv []string

	// Permission policy.
	PermissionMode     permission.Mode
	PermissionModeFunc func() permission.Mode
	Allowed            []string
	Disallowed         []string
	CanUseTool         permission.CanUseTool

	// Hooks is optional lifecycle instrumentation. Nil disables it.
	Hooks HookRunner
	// PendingMessages drains host-owned human text at safe stream/tool boundaries.
	// The callback must be concurrency-safe and must not return Queue messages.
	PendingMessages func(context.Context) []llm.Message
	// Compactor is optional context-window management. Nil disables it.
	Compactor     Compactor
	ContextWindow int // final FIFO request guard, including system/tools/output reserve
	// Recorder, when non-nil, is notified as the conversation grows so a host can
	// persist a transcript. It fires for genuinely new messages (births) in
	// order, and for compaction boundaries; it is never called for compaction's
	// ephemeral working-set edits (MicroCompact tool-result clearing), so the
	// recorded transcript is the raw, full-fidelity history.
	Recorder Recorder

	// TokenBudget, when > 0, enables budget-driven continuation: after a turn
	// with no tool calls, the agent is nudged to keep working until ~90% of the
	// budget (output tokens), stopping early on diminishing returns.
	TokenBudget int
	// EscalateMaxTokens, when true, first retries a max_tokens truncation with a
	// raised output cap (64k) before falling back to resume-recovery.
	EscalateMaxTokens bool

	// Settlement, when non-nil with a Prompt, runs a bounded wrap-up phase the
	// moment a budget (MaxTurns/MaxDuration) is hit: it injects Prompt as a user
	// turn and hides DisabledTools, so a budget-terminated run can checkpoint its
	// findings instead of ending abruptly. The run still finishes with the budget's
	// reason (ReasonMaxTurns/ReasonTimeout), never ReasonCompleted. Nil = off.
	Settlement *Settlement
}

// Settlement configures the wrap-up phase that runs when a budget is hit.
type Settlement struct {
	Prompt        string   // wrap-up instruction injected as a user turn (empty = settlement off)
	MaxTurns      int      // turn budget for the wrap-up phase itself (0 = default 2)
	DisabledTools []string // tool names hidden from the model during wrap-up (e.g. ["Bash"])
	// PromptByReason optionally overrides Prompt based on WHY the run settled
	// (keyed by the terminal reason, e.g. ReasonTimeout / ReasonMaxTurns). When the
	// settling reason has a non-empty entry here it is used; otherwise Prompt is used.
	// Lets a caller inject a different wrap-up depending on whether a run was cut by
	// its time budget vs its step budget, without knowing which at build time.
	PromptByReason map[TerminalReason]string
}

// promptFor returns the wrap-up prompt to inject for a settling reason: the
// PromptByReason override when present and non-empty, else the base Prompt.
func (s *Settlement) promptFor(reason TerminalReason) string {
	if s.PromptByReason != nil {
		if p := s.PromptByReason[reason]; p != "" {
			return p
		}
	}
	return s.Prompt
}

// Compactor is the harness's view of the compaction system (implemented in
// package compaction). Compaction is non-destructive; the harness sends only
// llm.MessagesForAPI(messages) to the model.
type Compactor interface {
	// Pre conditions the history before a model request. lastInputTokens is the
	// total token size the previous model response reported (0 if none yet), used
	// as the initial gate count in preference to local estimation.
	Pre(ctx context.Context, msgs []llm.Message, lastInputTokens int) []llm.Message
	Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool)
	IsOverflow(err error) bool
}

// RequestCompactor gates compression on the fully built request, rather than
// a stale previous input count that omits new messages and fixed schemas.
type RequestCompactor interface {
	PreRequest(ctx context.Context, msgs []llm.Message, inputTokens, reservedTokens int) []llm.Message
}

// ContextView is optionally implemented by a Compactor that builds the request
// message array itself, replacing llm.MessagesForAPI. Hosts with their own
// context manager (e.g. model-driven compression) implement it to project the
// history per request without mutating it.
//
// View MUST be pure: the returned slice is used for this request only and is
// never written back to the loop's history. Implementations that depend on
// stable message identity (content hashing, per-message refs) rely on this —
// writing back would change message bytes and invalidate that identity.
type ContextView interface {
	View(ctx context.Context, msgs []llm.Message) []llm.Message
}

// Recorder receives conversation events for durable transcript persistence
// (implemented in package transcript). RecordMessage fires once per new message
// in birth order; usage carries the token delta attributed to that turn
// (assistant messages only; zero otherwise). RecordBoundary fires when
// compaction inserts a boundary, for observability — boundaries are not part of
// the raw reconstructable history.
type Recorder interface {
	RecordMessage(m llm.Message, usage llm.Usage)
	RecordBoundary(meta llm.BoundaryMeta)
}

// HookRunner is the harness's view of the hook system (implemented in package
// hook). Stop returns two distinct outcomes: preventContinuation (hard
// terminate) and/or blockingErrors (inject + continue).
type HookRunner interface {
	PreToolUse(ctx context.Context, name string, input []byte) (block bool, message string, updated []byte)
	PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool)
	Stop(ctx context.Context, messages []llm.Message) (prevent bool, blockingErrors []string, message string)
}

// QueryDeps are injectable side-effect boundaries for testing (FR-01.6).
type QueryDeps struct {
	CallModel func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
	// CallModelSync is the non-streaming counterpart of CallModel, used when
	// QueryInput.NonStreaming is set. It performs a single request and returns the
	// fully assembled assistant message, the stop reason, and cumulative usage.
	CallModelSync func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
	NewID         func() string
}

func (d *QueryDeps) withDefaults(in QueryInput) {
	if d.CallModel == nil && in.Provider != nil {
		d.CallModel = in.Provider.Stream
	}
	if d.CallModelSync == nil && in.Provider != nil {
		d.CallModelSync = in.Provider.Complete
	}
	if d.NewID == nil {
		var n int64
		d.NewID = func() string { return "id_" + strconv.FormatInt(atomic.AddInt64(&n, 1), 10) }
	}
}

// Query runs the agent loop and returns its event stream. The final event is
// always KindResult carrying a Terminal (unless the consumer stops early).
func Query(ctx context.Context, in QueryInput, deps QueryDeps) iter.Seq2[Event, error] {
	deps.withDefaults(in)
	conc := in.MaxConcurrency
	if conc <= 0 {
		conc = 10
	}
	return func(yield func(Event, error) bool) {
		l := &loop{ctx: ctx, in: in, deps: deps, yield: yield, sem: make(chan struct{}, conc)}
		l.messages = append([]llm.Message(nil), in.Messages...)
		l.run()
	}
}

// budgetTracker tracks token-budget continuation state.
type budgetTracker struct {
	continuationCount    int
	lastGlobalTurnTokens int
	lastDeltaTokens      int
}

type loop struct {
	ctx context.Context
	// toolCtx bounds tool execution with the run's MaxDuration deadline, so a tool
	// that overruns the wall-clock budget is interrupted mid-flight (the model call
	// stays on ctx — streams are never cut mid-flight). Derived from ctx, so a parent
	// cancellation (pause/kill/shutdown) still cancels it. Swapped to ctx once the
	// wrap-up phase begins, so settlement tools run live, bounded only by settleMaxTurns.
	toolCtx context.Context
	in      QueryInput
	deps    QueryDeps
	yield   func(Event, error) bool
	sem     chan struct{} // bounds concurrent tool execution

	messages        []llm.Message
	usage           llm.Usage
	lastInputTokens int // total token size the previous model response reported (compaction gate)
	turnCount       int
	boundaryCount   int       // boundary markers already seen, so Recorder fires once each
	startTime       time.Time // loop start, for MaxDuration (wall-clock budget)

	// settlement (wrap-up) phase state, entered when a budget is hit
	settling     bool
	settleReason TerminalReason // the budget reason to finish with after wrap-up
	settleTurn   int

	// recovery / continuation state
	maxOutputTokensOverride      int
	maxOutputTokensRecoveryCount int
	hasAttemptedReactiveCompact  bool
	stopHookActive               bool
	budget                       budgetTracker
	lastContinue                 ContinueReason
}

const (
	budgetCompletionThresholdNum = 9 // 0.9
	budgetCompletionThresholdDen = 10
	budgetDiminishingThreshold   = 500
	escalatedMaxTokens           = 64000
	maxOutputTokensRecoveryLimit = 3
	// maxLoopIterations is a defensive backstop; turnCount counts only tool-turns,
	// so this only guards against a pathological recovery/continuation loop.
	maxLoopIterations = 10000
)

func (l *loop) emit(ev Event) bool { return l.yield(ev, nil) }

// record forwards a newly appended message to the transcript Recorder, if any.
func (l *loop) record(m llm.Message, u llm.Usage) {
	if l.in.Recorder != nil {
		l.in.Recorder.RecordMessage(m, u)
	}
}

// injectTaskNotifications drains completion notifications from the background-task
// manager and appends them to the conversation as a user message, so the model is
// told when background work finishes. Returns true if any were injected.
func (l *loop) injectTaskNotifications() bool {
	if l.in.Tasks == nil {
		return false
	}
	notes := l.in.Tasks.DrainNotifications()
	if len(notes) == 0 {
		return false
	}
	var b strings.Builder
	for i, n := range notes {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(n.String())
	}
	msg := llm.UserText(b.String())
	l.messages = append(l.messages, msg)
	l.record(msg, llm.Usage{})
	return true
}

// recordNewBoundary persists a boundary that compaction just inserted (detected
// by a rise in the boundary-marker count). It writes the post-boundary working
// set — the boundary marker, the summary, and the kept recent tail — as durable
// messages, so a COLD-START Resume restores the compacted view via MessagesForAPI
// instead of replaying raw history and re-summarizing. The tail is re-recorded
// after the boundary (it also exists earlier as births); only the post-boundary
// copy is sent to the model. RecordBoundary keeps the observability-only marker
// (a Type:"boundary" record, skipped during reconstruction).
func (l *loop) recordNewBoundary() {
	if l.in.Recorder == nil {
		return
	}
	c := countBoundaries(l.messages)
	if c <= l.boundaryCount {
		return
	}
	if i := llm.LastBoundaryIndex(l.messages); i >= 0 {
		if meta, ok := llm.ParseBoundaryMeta(l.messages[i]); ok {
			l.in.Recorder.RecordBoundary(meta)
		}
		for _, m := range l.messages[i:] { // boundary marker + summary + kept tail
			l.in.Recorder.RecordMessage(m, llm.Usage{})
		}
	}
	l.boundaryCount = c
}

func usageDelta(before, after llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:      after.InputTokens - before.InputTokens,
		OutputTokens:     after.OutputTokens - before.OutputTokens,
		CacheReadTokens:  after.CacheReadTokens - before.CacheReadTokens,
		CacheWriteTokens: after.CacheWriteTokens - before.CacheWriteTokens,
	}
}

func countBoundaries(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if llm.IsBoundaryMarker(m) {
			n++
		}
	}
	return n
}

func (l *loop) finish(reason TerminalReason, err error, text string) {
	l.yield(Event{Kind: KindResult, Terminal: &Terminal{
		Reason:   reason,
		Err:      err,
		Usage:    l.usage,
		Turns:    l.turnCount,
		Messages: l.messages,
		Text:     text,
	}}, nil)
}

func (l *loop) maxTokens() int {
	if l.maxOutputTokensOverride > 0 {
		return l.maxOutputTokensOverride
	}
	if l.in.MaxTokens > 0 {
		return l.in.MaxTokens
	}
	if l.in.ContextWindow > 0 {
		return min(32768, max(1, l.in.ContextWindow/4))
	}
	return 32768
}

func (l *loop) run() {
	var schemas []llm.ToolSchema
	if l.in.Tools != nil {
		schemas = l.in.Tools.Schemas()
		// Deferred tools stay in the registry (callable via ExecuteExtraTool) but
		// their schemas are withheld from the model to save context tokens.
		if len(l.in.DeferredTools) > 0 {
			schemas = filterToolSchemas(schemas, l.in.DeferredTools)
		}
	}
	l.turnCount = 1 // the first model call is turn 1
	l.boundaryCount = countBoundaries(l.messages)
	l.startTime = time.Now() // wall-clock baseline for MaxDuration
	// toolCtx carries the MaxDuration deadline so a tool that overruns the wall-clock
	// budget is interrupted mid-flight (rather than the run only noticing at the next
	// turn boundary, which a hung tool never reaches). On interrupt the loop enters the
	// wrap-up phase on the live parent ctx instead of aborting — see the tools branch.
	l.toolCtx = l.ctx
	if l.in.MaxDuration > 0 {
		var stopTool context.CancelFunc
		l.toolCtx, stopTool = context.WithDeadline(l.ctx, l.startTime.Add(l.in.MaxDuration))
		defer stopTool()
	}
	iterations := 0
	retryPolicy := modelRetryPolicy(l.ctx)
	consecutiveFailures := 0
	for {
		if l.ctx.Err() != nil {
			l.finish(ReasonAbortedStreaming, l.ctx.Err(), "")
			return
		}
		// Defensive backstop against a pathological recovery/continuation loop.
		// turnCount itself only counts tool-turns (below); every continuation path
		// is individually bounded, so this guard should never fire in practice.
		iterations++
		if iterations > maxLoopIterations {
			l.finish(ReasonMaxTurns, nil, "")
			return
		}

		if l.in.ContextFilter != nil {
			filtered, err := l.in.ContextFilter(l.messages)
			if err != nil {
				l.finish(ReasonModelError, err, "")
				return
			}
			l.messages = filtered
			l.boundaryCount = countBoundaries(l.messages)
		}
		l.injectPendingMessages()
		// Non-destructive proactive compaction (MicroCompact → AutoCompact).
		if l.in.Compactor != nil {
			l.messages = l.in.Compactor.Pre(l.ctx, l.messages, l.lastInputTokens)
			if l.in.ContextFilter != nil {
				filtered, err := l.in.ContextFilter(l.messages)
				if err != nil {
					l.finish(ReasonModelError, err, "")
					return
				}
				l.messages = filtered
				if count := countBoundaries(filtered); count < l.boundaryCount {
					l.boundaryCount = count
				}
			}
			l.recordNewBoundary()
		}

		usageBefore := l.usage
		asst, exec, stopReason, streamErr, consumerStopped, aborted := l.streamAndExecute(schemas)
		if consumerStopped {
			return
		}
		if streamErr != nil {
			if l.ctx.Err() != nil {
				l.finish(ReasonAbortedStreaming, l.ctx.Err(), "")
				return
			}
			// Reactive recovery from prompt-too-long: one forced AutoCompact, then retry.
			if errors.Is(streamErr, ErrContextWindow) {
				l.finish(ReasonPromptTooLong, streamErr, "")
				return
			}
			if l.in.Compactor != nil && l.in.Compactor.IsOverflow(streamErr) && !l.hasAttemptedReactiveCompact {
				l.hasAttemptedReactiveCompact = true
				if msgs, ok := l.in.Compactor.Reactive(l.ctx, l.messages); ok {
					l.messages = msgs
					if l.in.ContextFilter != nil {
						filtered, err := l.in.ContextFilter(l.messages)
						if err != nil {
							l.finish(ReasonModelError, err, "")
							return
						}
						l.messages = filtered
						if count := countBoundaries(filtered); count < l.boundaryCount {
							l.boundaryCount = count
						}
					}
					l.recordNewBoundary()
					l.lastContinue = ContinueReactiveCompactRetry
					continue
				}
				l.finish(ReasonPromptTooLong, streamErr, "")
				return
			}
			// Tools may have completed while the stream was still arriving. Drain
			// them and record their exact call/result pairs before retrying; never
			// re-run a previously executed command or fabricate partial arguments.
			if exec != nil && exec.count() > 0 {
				cs, _ := exec.finish()
				if cs {
					return
				}
				asst.Content = exec.executedCalls()
				l.messages = append(l.messages, asst)
				l.record(asst, usageDelta(usageBefore, l.usage))
				result := llm.Message{Role: llm.RoleUser, Content: exec.results()}
				l.messages = append(l.messages, result)
				l.record(result, llm.Usage{})
				for _, extra := range exec.extraMessages() {
					l.messages = append(l.messages, extra)
					l.record(extra, llm.Usage{})
				}
			} else if u := usageDelta(usageBefore, l.usage); u != (llm.Usage{}) {
				// Retain billed usage from failed attempts without replaying an
				// incomplete assistant message or unfinished tool arguments.
				l.record(llm.Message{Role: llm.RoleAssistant}, u)
			}
			if u := l.usage; u != (llm.Usage{}) {
				if !l.emit(Event{Kind: KindUsage, Usage: &u}) {
					return
				}
			}
			if consecutiveFailures < retryPolicy.Retries && retryPolicy.Retryable != nil && retryPolicy.Retryable(streamErr) {
				consecutiveFailures++
				if !l.emit(Event{Kind: KindRetry, Text: fmt.Sprintf("模型调用失败，等待 %s 后重试（%d/%d）\n上游错误：\n%s", retryPolicy.Delay, consecutiveFailures, retryPolicy.Retries, streamErr.Error())}) {
					return
				}
				timer := time.NewTimer(retryPolicy.Delay)
				select {
				case <-l.toolCtx.Done():
					timer.Stop()
					if l.ctx.Err() != nil {
						l.finish(ReasonAbortedStreaming, l.ctx.Err(), "")
						return
					}
					if !l.beginSettlement(ReasonTimeout, &schemas) {
						l.finish(ReasonTimeout, nil, "")
						return
					}
					l.toolCtx = l.ctx
				case <-timer.C:
				}
				continue
			}
			l.finish(ReasonModelError, streamErr, "")
			return
		}
		consecutiveFailures = 0
		l.hasAttemptedReactiveCompact = false
		l.messages = append(l.messages, asst)
		turnUsage := usageDelta(usageBefore, l.usage)
		// The size the model just reported for its input, preferred by the
		// compactor over local estimation on the next turn's gate check.
		l.lastInputTokens = turnUsage.InputTokens + turnUsage.OutputTokens
		l.record(asst, turnUsage)
		// surface cumulative usage after each model turn so hosts can show live token
		// counts during a run (not just at the terminal event).
		if u := l.usage; u != (llm.Usage{}) {
			if !l.emit(Event{Kind: KindUsage, Usage: &u}) {
				return
			}
		}
		toolUses := asst.ToolUses()

		// max_tokens truncation with no tool calls → escalate / resume-recovery.
		// These continuations are retries of the same turn (they do not advance
		// turnCount).
		if stopReason == "max_tokens" && len(toolUses) == 0 {
			if l.handleMaxOutputTokens() {
				continue
			}
			// recovery exhausted: surface the truncated turn as completion.
		} else {
			l.maxOutputTokensOverride = 0
		}

		if len(toolUses) == 0 {
			// During the wrap-up phase, a natural stop ends the run with the ORIGINAL
			// budget reason (not "completed") — the run was budget-terminated; the
			// settlement is just a checkpoint. Skip the keep-working nudges below.
			if l.settling {
				l.finish(l.settleReason, nil, asst.Text())
				return
			}
			if l.injectPendingMessages() {
				l.turnCount++
				l.lastContinue = ContinueNextTurn
				continue
			}
			// Stop hooks: hard terminate, or force continue with injected errors.
			// stopHookActive prevents an always-blocking hook from looping forever
			// without an intervening tool-turn.
			if l.in.Hooks != nil && !l.stopHookActive {
				prevent, blocking, msg := l.in.Hooks.Stop(l.ctx, l.messages)
				if prevent {
					l.finish(ReasonStopHookPrevented, nil, msg)
					return
				}
				if len(blocking) > 0 {
					for _, b := range blocking {
						bm := llm.UserText(b)
						l.messages = append(l.messages, bm)
						l.record(bm, llm.Usage{})
					}
					l.stopHookActive = true
					l.lastContinue = ContinueStopHookBlocking
					continue
				}
			}
			l.stopHookActive = false
			// Surface any background-task completions before ending the turn; if any
			// landed, take another turn so the model can react to them.
			if l.injectTaskNotifications() {
				l.lastContinue = ContinueTaskNotification
				continue
			}
			// Token budget: nudge to keep working until ~90% / diminishing returns.
			if l.in.TokenBudget > 0 && l.checkBudgetContinue() {
				l.lastContinue = ContinueTokenBudget
				continue
			}
			l.finish(ReasonCompleted, nil, asst.Text())
			return
		}

		// Tools were executed during streaming; record their results.
		results := exec.results()
		resultMsg := llm.Message{Role: llm.RoleUser, Content: results}
		l.messages = append(l.messages, resultMsg)
		l.record(resultMsg, llm.Usage{})
		// Extra messages a tool injected after its result (e.g. the Skill tool's
		// instructions as an independent user message) are appended next so the
		// next model round sees them as standing guidance.
		for _, em := range exec.extraMessages() {
			l.messages = append(l.messages, em)
			l.record(em, llm.Usage{})
		}
		// Surface any background-task completions that landed during this turn so
		// the next model round sees them alongside the tool results.
		l.injectTaskNotifications()
		// Parent-ctx cancellation (pause / planner kill / shutdown) → real abort, no wrap-up.
		if l.ctx.Err() != nil {
			l.finish(ReasonAbortedTools, l.ctx.Err(), "")
			return
		}
		// aborted with the parent still alive ⇒ the run's own MaxDuration deadline (on
		// toolCtx) interrupted an in-flight tool. Enter the wrap-up phase on the live
		// parent ctx instead of dying, so a run whose tool overran the wall-clock budget
		// still checkpoints its findings. Mid-tool interrupts are always time-driven
		// (MaxTurns is only ever checked at the turn boundary), hence ReasonTimeout.
		if aborted {
			if l.settling {
				l.finish(l.settleReason, nil, "")
				return
			}
			if l.beginSettlement(ReasonTimeout, &schemas) {
				l.toolCtx = l.ctx // wrap-up tools run live, bounded only by settleMaxTurns
				l.lastContinue = ContinueNextTurn
				continue
			}
			l.finish(ReasonTimeout, nil, "")
			return
		}

		// next_turn: only tool-turns advance turnCount.
		l.stopHookActive = false // a real turn happened; stop hooks may fire fresh
		if l.settling {
			// wrap-up phase: bounded by its own small turn budget. When spent, finish
			// with the original budget reason (not "completed").
			l.settleTurn++
			if l.settleTurn >= l.settleMaxTurns() {
				l.finish(l.settleReason, nil, "")
				return
			}
			l.lastContinue = ContinueNextTurn
			continue
		}
		// Primary budgets are checked HERE, at the turn boundary — never mid-stream —
		// so a turn is not aborted in flight (that would surface as an abort/error).
		// On hit, optionally run the wrap-up phase before finishing.
		nextTurn := l.turnCount + 1
		hitTurns := l.in.MaxTurns > 0 && nextTurn > l.in.MaxTurns
		hitTime := l.in.MaxDuration > 0 && time.Since(l.startTime) >= l.in.MaxDuration
		if hitTurns || hitTime {
			reason := ReasonMaxTurns
			if hitTime && !hitTurns {
				reason = ReasonTimeout
			}
			if l.beginSettlement(reason, &schemas) {
				l.lastContinue = ContinueNextTurn
				continue
			}
			l.finish(reason, nil, "")
			return
		}
		l.turnCount = nextTurn
		l.lastContinue = ContinueNextTurn
	}
}

// settleMaxTurns is the wrap-up phase's own turn budget (default 2).
func (l *loop) settleMaxTurns() int {
	if l.in.Settlement != nil && l.in.Settlement.MaxTurns > 0 {
		return l.in.Settlement.MaxTurns
	}
	return 2
}

// beginSettlement enters the wrap-up phase when configured: injects the caller's
// prompt as a user turn and hides DisabledTools, so a budget-terminated run can
// checkpoint its findings. Returns false when no settlement is configured (caller
// then finishes immediately).
func (l *loop) beginSettlement(reason TerminalReason, schemas *[]llm.ToolSchema) bool {
	s := l.in.Settlement
	if s == nil {
		return false
	}
	prompt := s.promptFor(reason)
	if prompt == "" {
		return false
	}
	msg := llm.UserText(prompt)
	l.messages = append(l.messages, msg)
	l.record(msg, llm.Usage{})
	l.settling = true
	l.settleReason = reason
	l.settleTurn = 0
	if len(s.DisabledTools) > 0 {
		*schemas = filterToolSchemas(*schemas, s.DisabledTools)
	}
	return true
}

// filterToolSchemas returns schemas with the named tools removed.
func filterToolSchemas(schemas []llm.ToolSchema, disabled []string) []llm.ToolSchema {
	block := make(map[string]bool, len(disabled))
	for _, d := range disabled {
		block[d] = true
	}
	out := make([]llm.ToolSchema, 0, len(schemas))
	for _, s := range schemas {
		if !block[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// handleMaxOutputTokens implements the escalate→recover(3×) flow. Returns true
// if it injected a recovery and the loop should continue.
func (l *loop) handleMaxOutputTokens() bool {
	if l.in.EscalateMaxTokens && l.maxOutputTokensOverride == 0 {
		l.maxOutputTokensOverride = escalatedMaxTokens
		l.lastContinue = ContinueMaxOutputTokensEscalate
		return true
	}
	if l.maxOutputTokensRecoveryCount < maxOutputTokensRecoveryLimit {
		nudge := llm.UserText(
			"Output token limit hit. Resume directly — no apology, no recap of what you already wrote. Continue exactly where you left off.")
		l.messages = append(l.messages, nudge)
		l.record(nudge, llm.Usage{})
		l.maxOutputTokensRecoveryCount++
		l.maxOutputTokensOverride = 0
		l.lastContinue = ContinueMaxOutputTokensRecovery
		return true
	}
	return false
}

// checkBudgetContinue continues with a nudge while under 90% of budget and not
// in diminishing returns; otherwise stops.
func (l *loop) checkBudgetContinue() bool {
	budget := l.in.TokenBudget
	if budget <= 0 || l.in.AgentID != "" { // subagents have no budget
		return false
	}
	turnTokens := l.usage.OutputTokens
	delta := turnTokens - l.budget.lastGlobalTurnTokens
	diminishing := l.budget.continuationCount >= 3 && delta < budgetDiminishingThreshold && l.budget.lastDeltaTokens < budgetDiminishingThreshold
	if !diminishing && turnTokens < budget*budgetCompletionThresholdNum/budgetCompletionThresholdDen {
		l.budget.continuationCount++
		l.budget.lastDeltaTokens = delta
		l.budget.lastGlobalTurnTokens = turnTokens
		pct := 0
		if budget > 0 {
			pct = turnTokens * 100 / budget
		}
		nudge := llm.UserText(fmt.Sprintf(
			"You have used %d%% of your token budget for this task (%d/%d output tokens). Keep going and finish the task; wrap up before the budget is exhausted.",
			pct, turnTokens, budget))
		l.messages = append(l.messages, nudge)
		l.record(nudge, llm.Usage{})
		return true
	}
	return false
}

// Todo system-reminder throttle defaults (mirror claude-code's TODO_REMINDER_CONFIG).
const (
	defaultTodoTurnsSinceWrite = 10
	defaultTodoTurnsBetween    = 10
)

func (l *loop) todoTurnsSinceWrite() int {
	if l.in.TodoReminderTurnsSinceWrite > 0 {
		return l.in.TodoReminderTurnsSinceWrite
	}
	return defaultTodoTurnsSinceWrite
}

func (l *loop) todoTurnsBetween() int {
	if l.in.TodoReminderTurnsBetweenReminder > 0 {
		return l.in.TodoReminderTurnsBetweenReminder
	}
	return defaultTodoTurnsBetween
}

// maybeInjectTodoReminder appends a todo <system-reminder> user message to the
// history when the model has gone long enough without touching TodoWrite and
// without a recent reminder. Throttle turns are derived from the API-visible
// window (post-boundary) so a reminder re-appears after compaction drops the
// prior one. No-op when todos are disabled or the list is empty.
func (l *loop) maybeInjectTodoReminder() {
	if l.in.Todos == nil {
		return
	}
	todos := l.in.Todos.List()
	if len(todos) == 0 {
		return
	}
	sinceWrite, sinceReminder := todoReminderTurnCounts(llm.MessagesForAPI(l.messages))
	if sinceWrite < l.todoTurnsSinceWrite() || sinceReminder < l.todoTurnsBetween() {
		return
	}
	body := tool.RenderTodoReminder(todos)
	if body == "" {
		return
	}
	msg := llm.TodoReminderMessage(body)
	l.messages = append(l.messages, msg)
	l.record(msg, llm.Usage{})
}

// todoReminderTurnCounts scans messages (most-recent first) and returns the
// number of assistant turns since the last TodoWrite tool_use and since the last
// todo reminder. The turn that contains the TodoWrite is not itself counted (so
// the call turn reads as 0). A never-seen event yields the total assistant-turn
// count (effectively "long ago").
func todoReminderTurnCounts(msgs []llm.Message) (sinceWrite, sinceReminder int) {
	foundWrite, foundReminder := false, false
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if !foundReminder && llm.IsTodoReminder(m) {
			foundReminder = true
			continue
		}
		if m.Role != llm.RoleAssistant {
			continue
		}
		if !foundWrite {
			if messageHasToolUse(m, "TodoWrite") {
				foundWrite = true
			} else {
				sinceWrite++
			}
		}
		if !foundReminder {
			sinceReminder++
		}
	}
	return
}

func messageHasToolUse(m llm.Message, name string) bool {
	for _, b := range m.Content {
		if b.Type == llm.BlockToolUse && b.Name == name {
			return true
		}
	}
	return false
}

// injectPendingMessages is a host-owned safe-boundary inbox. It never cancels
// the current stream/tool batch and cannot introduce fabricated tool results.
func (l *loop) injectPendingMessages() bool {
	if l.in.PendingMessages == nil || l.ctx.Err() != nil || l.settling {
		return false
	}
	if l.in.MaxTurns > 0 && l.turnCount >= l.in.MaxTurns {
		return false
	}
	if l.in.MaxDuration > 0 && time.Since(l.startTime) >= l.in.MaxDuration {
		return false
	}
	added := false
	for _, message := range l.in.PendingMessages(l.ctx) {
		if message.Role != llm.RoleUser || len(message.Content) == 0 {
			continue
		}
		valid := true
		for _, block := range message.Content {
			if block.Type != llm.BlockText && !(block.Type == llm.BlockImage && block.Source != nil) {
				valid = false
			}
		}
		if !valid {
			continue
		}
		l.messages = append(l.messages, message)
		l.record(message, llm.Usage{})
		added = true
	}
	return added
}

// requestMessages builds the message array for one model request. A Compactor
// that implements ContextView takes over the projection entirely; otherwise the
// default working set (post-boundary, unpaired tool blocks dropped) is used.
// Either way l.messages is left untouched.
func (l *loop) requestMessages() []llm.Message {
	if v, ok := l.in.Compactor.(ContextView); ok {
		return v.View(l.ctx, l.messages)
	}
	return llm.MessagesForAPI(l.messages)
}

// streamAndExecute runs one model completion, surfacing text/thinking, and feeds
// tool_use blocks to the streaming executor as they finish — so tools begin
// running while the rest of the message is still arriving. It returns the
// assembled assistant message, the executor, the stop reason, and flags.
func (l *loop) streamAndExecute(schemas []llm.ToolSchema) (asst llm.Message, exec *streamExec, stopReason string, err error, consumerStopped, aborted bool) {
	l.maybeInjectTodoReminder()
	msgs := l.requestMessages()
	if l.in.SystemReminder != "" {
		msgs = append([]llm.Message{llm.UserText(l.in.SystemReminder)}, msgs...)
	}
	req := llm.CompletionRequest{
		System:          l.in.System,
		DynamicBoundary: l.in.DynamicBoundary,
		Messages:        msgs,
		Tools:           schemas,
		MaxTokens:       l.maxTokens(),
		Temperature:     l.in.Temperature,
	}
	if l.in.ContextWindow > 0 {
		if c, ok := l.in.Compactor.(RequestCompactor); ok {
			before, sizeErr := measureContextRequest(req, l.in.ContextWindow)
			if sizeErr != nil {
				err = fmt.Errorf("%w: cannot size request: %v", ErrContextWindow, sizeErr)
				return
			}
			// Publish the pending input BEFORE a potentially lengthy summary. If
			// cancelled during compaction the UI must not retain the old small request.
			if before.UsedTokens+before.ReservedTokens >= l.in.ContextWindow*90/100 {
				before.Phase = "before_compaction"
				if !l.emit(Event{Kind: KindContextUsage, Context: &before}) {
					consumerStopped = true
					return
				}
			}
			l.messages = c.PreRequest(l.ctx, l.messages, before.UsedTokens, before.ReservedTokens)
			if l.ctx.Err() != nil {
				err = l.ctx.Err()
				return
			}
			if l.in.ContextFilter != nil {
				l.messages, err = l.in.ContextFilter(l.messages)
				if err != nil {
					return
				}
			}
			l.recordNewBoundary()
			req.Messages = l.requestMessages()
			if l.in.SystemReminder != "" {
				req.Messages = append([]llm.Message{llm.UserText(l.in.SystemReminder)}, req.Messages...)
			}
		}
		protectedPrefix := 0
		if l.in.SystemReminder != "" {
			protectedPrefix = 1
		}
		var usage ContextUsage
		req, usage, err = trimContextRequest(req, l.in.ContextWindow, protectedPrefix)
		if !l.emit(Event{Kind: KindContextUsage, Context: &usage}) {
			consumerStopped = true
			return
		}
		if err != nil {
			return
		}
	}
	if l.in.NonStreaming {
		return l.completeAndExecute(req)
	}
	exec = newStreamExec(l)
	acc := llm.NewAccumulator()
	terminalSeen := false

	// A stream may announce several parallel calls before any of their argument
	// fragments arrive (the Responses API emits one output_item.added per call
	// first, then function_call_arguments.delta frames keyed by item_id). Keep
	// one slot per call, route fragments/stops by that key, and finalize each
	// call as its own completion arrives — a single "current call" slot garbles
	// parallel calls whose starts are batched. Providers that never interleave
	// keep the legacy single-slot behavior through the fallback slot below.
	type pendingCall struct {
		id, name string
		key      string
		input    strings.Builder
		stopText string // authoritative full arguments from a tool_use_stop
	}
	var calls []*pendingCall // arrival order; also the dispatch order
	byKey := map[string]*pendingCall{}
	var last *pendingCall

	routeInput := func(itemID string) *pendingCall {
		if itemID != "" {
			if p := byKey[itemID]; p != nil {
				return p
			}
		}
		return last // legacy fallback: most recently started call
	}
	routeStop := func(itemID string) *pendingCall {
		if itemID != "" {
			return byKey[itemID] // exact match only; unknown keys are no-ops
		}
		return last
	}
	drop := func(p *pendingCall) {
		for i, q := range calls {
			if q == p {
				calls = append(calls[:i], calls[i+1:]...)
				break
			}
		}
		if p.key != "" {
			delete(byKey, p.key)
		}
		if last == p {
			last = nil
		}
	}
	finalizeCall := func(p *pendingCall) bool { // false = consumer stopped
		raw := strings.TrimSpace(p.input.String())
		if raw == "" || !jsonValid(raw) {
			if alt := strings.TrimSpace(p.stopText); alt != "" && jsonValid(alt) {
				raw = alt
			}
		}
		if raw == "" {
			raw = "{}"
		}
		if !jsonValid(raw) {
			err = fmt.Errorf("incomplete or invalid arguments for tool %s", p.name)
			return true // do not execute fabricated arguments
		}
		block := llm.ContentBlock{Type: llm.BlockToolUse, ID: p.id, Name: p.name, Input: []byte(raw)}
		if !l.emit(Event{Kind: KindToolUse, ToolUse: &block}) {
			return false
		}
		exec.add(block)
		return exec.drain()
	}
	finalizeAll := func() bool {
		for len(calls) > 0 {
			p := calls[0]
			if !finalizeCall(p) {
				return false
			}
			drop(p)
		}
		return true
	}
	// flushComplete finalizes already-complete calls when a new one starts, so
	// sequential streams still dispatch tools mid-stream (FR-01.4); calls whose
	// fragments have not arrived yet (batched parallel starts) stay pending.
	flushComplete := func() bool {
		for _, p := range append([]*pendingCall(nil), calls...) {
			if raw := strings.TrimSpace(p.input.String()); raw != "" && jsonValid(raw) {
				if !finalizeCall(p) {
					return false
				}
				drop(p)
			}
		}
		return true
	}

	for ev, e := range l.deps.CallModel(l.ctx, req) {
		if e != nil {
			err = e
			break
		}
		if ev.Type == llm.SEMessageStop || (ev.Type == llm.SEMessageDelta && ev.StopReason != "") {
			terminalSeen = true
		}
		switch ev.Type {
		case llm.SEToolInputJSON:
			if p := routeInput(ev.ItemID); p != nil {
				p.input.WriteString(ev.Text)
			}
			acc.Add(ev)
			continue
		case llm.SEToolUseStop:
			if p := routeStop(ev.ItemID); p != nil {
				if ev.Text != "" {
					p.stopText = ev.Text
				}
				if !finalizeCall(p) {
					return asst, exec, "", nil, true, false
				}
				drop(p)
			}
			acc.Add(ev)
			continue
		}
		switch ev.Type {
		case llm.SETextDelta:
			if !flushComplete() {
				return asst, exec, "", nil, true, false
			}
			if ev.Text != "" && !l.emit(Event{Kind: KindText, Text: ev.Text}) {
				return asst, exec, "", nil, true, false
			}
		case llm.SEThinkingDelta:
			if !flushComplete() {
				return asst, exec, "", nil, true, false
			}
			if ev.Text != "" && !l.emit(Event{Kind: KindThinking, Text: ev.Text}) {
				return asst, exec, "", nil, true, false
			}
		case llm.SEToolUseStart:
			key := ev.ItemID
			if key == "" {
				key = ev.ToolID
			}
			p := &pendingCall{id: ev.ToolID, name: ev.ToolName, key: key}
			calls = append(calls, p)
			if key != "" {
				byKey[key] = p
			}
			last = p
			if !flushComplete() {
				return asst, exec, "", nil, true, false
			}
		default:
			if !flushComplete() {
				return asst, exec, "", nil, true, false
			}
		}
		acc.Add(ev)
	}
	if err == nil && !terminalSeen && ModelRetryManaged(l.ctx) {
		err = io.ErrUnexpectedEOF
	}
	// A failed stream must NOT execute unfinished tool calls (e.g. {} after
	// truncated JSON). Already finalized calls are retained by the retry loop.
	if err == nil && !finalizeAll() {
		return asst, exec, "", nil, true, false
	}
	l.usage.Add(acc.Usage)
	asst = acc.Message()
	stopReason = acc.StopReason
	if err == nil && ModelRetryManaged(l.ctx) && len(asst.Content) == 0 && stopReason != "max_tokens" {
		err = fmt.Errorf("model returned an empty completion")
	}
	if err != nil {
		return asst, exec, stopReason, err, false, false
	}
	// Drain tools to completion (or synthesize on abort).
	cs, ab := exec.finish()
	if cs {
		return asst, exec, stopReason, nil, true, false
	}
	return asst, exec, stopReason, nil, false, ab
}

// completeAndExecute is the non-streaming counterpart of streamAndExecute's body.
// It performs a single non-streaming model call, then replays the assembled
// message as host events (thinking → text → each tool_use) and runs the same
// tool executor. The returned tuple matches streamAndExecute so the turn loop is
// oblivious to which path produced it. Usage is folded into l.usage exactly as
// the streaming accumulator does, so the loop's per-turn usage delta, KindUsage
// emission, and recorder all see identical numbers.
func (l *loop) completeAndExecute(req llm.CompletionRequest) (asst llm.Message, exec *streamExec, stopReason string, err error, consumerStopped, aborted bool) {
	exec = newStreamExec(l)
	msg, sr, usage, cerr := l.deps.CallModelSync(l.ctx, req)
	if cerr != nil {
		return asst, exec, "", cerr, false, false
	}
	l.usage.Add(usage)
	asst = msg
	stopReason = sr
	if ModelRetryManaged(l.ctx) && (sr == "" || (len(msg.Content) == 0 && sr != "max_tokens")) {
		return asst, exec, sr, fmt.Errorf("model returned an empty or unterminated completion"), false, false
	}
	for _, b := range msg.Content {
		switch b.Type {
		case llm.BlockThinking:
			if b.Thinking != "" && !l.emit(Event{Kind: KindThinking, Text: b.Thinking}) {
				return asst, exec, stopReason, nil, true, false
			}
		case llm.BlockText:
			if b.Text != "" && !l.emit(Event{Kind: KindText, Text: b.Text}) {
				return asst, exec, stopReason, nil, true, false
			}
		case llm.BlockToolUse:
			block := b
			if !l.emit(Event{Kind: KindToolUse, ToolUse: &block}) {
				return asst, exec, stopReason, nil, true, false
			}
			exec.add(block)
			if !exec.drain() { // surface any results ready so far (FIFO)
				return asst, exec, stopReason, nil, true, false
			}
		}
	}
	// Drain tools to completion (or synthesize on abort).
	cs, ab := exec.finish()
	if cs {
		return asst, exec, stopReason, nil, true, false
	}
	return asst, exec, stopReason, nil, false, ab
}

func jsonValid(s string) bool {
	return json.Valid([]byte(s))
}
