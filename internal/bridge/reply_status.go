package bridge

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

// A reply may be built while mu is held (including approval cards), or by a
// sender/callback outside it. Publish immutable display data at state changes;
// rendering must never take mu or query the model catalog.
type replyStatus struct {
	thread, status, model, effort, modelLabel string
	queued                                    map[string]int
}

func (b *Bridge) taskStatusLocked(a *task) string {
	if a.resultStatus != "" {
		return a.resultStatus
	}
	if a.stopping {
		return "停止中"
	}
	for _, p := range b.pending {
		if p.thread == a.thread && p.turn == a.turn {
			return "等待审批或回答"
		}
	}
	if a.turn == "" {
		return "启动中"
	}
	return "执行中"
}

func (b *Bridge) sessionActivityStatus(thread string) string {
	if v := b.replyStatus.Load(); v != nil {
		if v.thread == thread && thread != "" {
			return v.status
		}
		if v.queued[thread] > 0 {
			return "排队中"
		}
	}
	return "空闲"
}

func (b *Bridge) sessionDisplay(s state.Session) string {
	project := filepath.Base(s.Cwd)
	if s.Cwd == "" {
		project = "未指定"
	}
	return b.sessionStatusLine(s) + "\n项目：" + project + " · 状态快照：" + time.Now().Format("15:04:05")
}

// Caller holds mu. Do not retain pointers into mutable tasks or queues.
func (b *Bridge) refreshReplyStatusLocked() {
	v := &replyStatus{queued: make(map[string]int)}
	for _, q := range b.queue {
		v.queued[q.Input.ThreadID]++
	}
	if a := b.active; a != nil {
		v.thread, v.model, v.effort = a.thread, a.modelSettings.Model, a.modelSettings.Effort
		v.status, v.modelLabel = "执行中", "当前轮"
		if a.turn == "" {
			v.status, v.modelLabel = "启动中", "本轮已选，待确认"
		}
		for _, p := range b.pending {
			if p.thread == a.thread && p.turn == a.turn {
				v.status = "等待审批或回答"
			}
		}
		if a.stopping {
			v.status = "停止中"
		}
		if a.resultStatus != "" {
			v.status = a.resultStatus
		}
		v.status = b.taskStatusLocked(a)
	}
	b.replyStatus.Store(v)
}

func (b *Bridge) sessionStatusLine(s state.Session) string {
	if s.OriginHome != "" || !b.workspaceAllowed(s.Cwd) {
		return "状态：外部只读 · 模型与思考强度：未读取"
	}
	status, label := "空闲", "下轮设置"
	global, overrides := b.store.ModelPreferences()
	settings := overrides[s.ThreadID]
	model, effort := settings.Model, settings.Effort
	if model == "" {
		model = global.Model
	}
	if model == "" {
		model = b.cfg.Model
	}
	if effort == "" {
		effort = global.Effort
	}
	if v := b.replyStatus.Load(); v != nil {
		if v.queued[s.ThreadID] > 0 {
			status = fmt.Sprintf("排队中（%d条）", v.queued[s.ThreadID])
		}
		if v.thread == s.ThreadID && v.thread != "" {
			status, label, model, effort = v.status, v.modelLabel, v.model, v.effort
			if v.queued[s.ThreadID] > 0 {
				status += fmt.Sprintf(" · 排队%d条", v.queued[s.ThreadID])
			}
		}
	}
	if model == "" {
		if label == "下轮设置" {
			model = "继承默认"
		} else {
			model = "待确认"
		}
	}
	if effort == "" {
		if label == "下轮设置" {
			effort = "继承默认"
		} else {
			effort = "待确认"
		}
	}
	return fmt.Sprintf("状态：%s\n%s · 模型：%s · 思考强度：%s", status, label, model, effort)
}
