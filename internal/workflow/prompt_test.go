package workflow

import (
	"strings"
	"testing"

	"ys-sandbox/internal/store/pg"
)

// M5 注入（spec Q5）：system prompt 的组装与截断。
// 纯函数直测；PG 接线由 workflow 级测试覆盖。

func TestBuildSystemPrompt_Assembly(t *testing.T) {
	out := buildSystemPrompt("BASE",
		[]pg.Memory{{Text: "偏好简短", Importance: 9}},
		[]pg.Memory{{Text: "2026-11-02 搬到 Seattle"}},
		[]pg.Memory{{Text: "在调试 15051 端口"}},
		[]pg.SkillCatalogEntry{{Name: "kill-152", Description: "杀进程的正确姿势"}},
	)
	for _, want := range []string{"BASE", "偏好简短", "2026-11-02 搬到 Seattle", "在调试 15051 端口", "kill-152", "杀进程的正确姿势"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt 缺 %q:\n%s", want, out)
		}
	}
}

func TestBuildSystemPrompt_EmptySectionsOmitted(t *testing.T) {
	out := buildSystemPrompt("BASE", nil, nil, nil, nil)
	if out != "BASE" {
		t.Fatalf("全空时应只有 base: %q", out)
	}
}

func TestBuildSystemPrompt_ProfileTruncated(t *testing.T) {
	// 每条 50 字符 × 200 条 = 10000，必须截到 4000 字符上限内
	var prof []pg.Memory
	for i := 0; i < 200; i++ {
		prof = append(prof, pg.Memory{Text: strings.Repeat("x", 50), Importance: 5})
	}
	out := buildSystemPrompt("BASE", prof, nil, nil, nil)
	if len(out) > 4000+len("BASE")+200 { // 200 = 节标题/截断标记的宽松量
		t.Fatalf("prompt 超长: %d", len(out))
	}
	if !strings.Contains(out, "截断") {
		t.Fatal("截断应有标记（记忆不只这些）")
	}
}
