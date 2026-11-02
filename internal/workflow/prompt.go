// M5 注入（spec Q5）：每个 turn 构建 system prompt = base + 记忆装配。
// profile 全量（≤100 条/4000 字符截断）+ log 最近 30 + 活跃 note + skills 清单（不带全文）。
// 讲座 30 的 Prompt 预算：Grok Bot 是 100 facts/4000 chars，我们沿用同量级。
package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"ys-sandbox/internal/store/pg"
)

// profilePromptMax 注入的 profile 块字符上限（超出截断并留标记——记忆不只这些，可用 memory_search 翻）。
const profilePromptMax = 4000

// buildSystemPrompt 纯组装：空块不占位；profile 超限截断。
func buildSystemPrompt(base string, profile, logs, notes []pg.Memory, skills []pg.SkillCatalogEntry) string {
	var b strings.Builder
	b.WriteString(base)

	if len(profile) > 0 {
		b.WriteString("\n\n# 关于用户的记忆\n")
		n := 0
		truncated := false
		for _, m := range profile {
			line := "- " + m.Text + "\n"
			if n+len(line) > profilePromptMax {
				truncated = true
				break
			}
			b.WriteString(line)
			n += len(line)
		}
		if truncated {
			b.WriteString("（更多记忆已截断，可用 memory_search 检索）\n")
		}
	}
	if len(logs) > 0 {
		b.WriteString("\n# 最近的事件\n")
		for _, m := range logs {
			b.WriteString("- " + m.Text + "\n")
		}
	}
	if len(notes) > 0 {
		b.WriteString("\n# 眼下进行中的事\n")
		for _, m := range notes {
			b.WriteString("- " + m.Text + "\n")
		}
	}
	if len(skills) > 0 {
		b.WriteString("\n# 可用技能（用 memory_read 工具读全文）\n")
		for _, s := range skills {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
	}
	return b.String()
}

// BuildSystemPrompt 注入装配 activity：session → user → 四路查询 → 组装。
// 每个 turn 都跑——记忆变了下一 turn 就生效（ spec Q5）。
func (a *Activities) BuildSystemPrompt(ctx context.Context, sessionID string) (string, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return "", err
	}
	sess, err := a.PG.GetSession(ctx, sid)
	if err != nil {
		return "", err
	}
	uid := sess.UserID
	profile, err := a.PG.ListProfileMemories(ctx, uid)
	if err != nil {
		return "", err
	}
	logs, err := a.PG.ListRecentLogMemories(ctx, uid)
	if err != nil {
		return "", err
	}
	notes, err := a.PG.ListActiveNotes(ctx, uid)
	if err != nil {
		return "", err
	}
	skills, err := a.PG.ListSkillCatalog(ctx, uid)
	if err != nil {
		return "", err
	}
	return buildSystemPrompt(a.BasePrompt, profile, logs, notes, skills), nil
}
