package workflow

import (
	"strings"
	"testing"
	"unicode/utf8"

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

// S5：中文按 rune 截断，不按字节——4000 rune 的中文 ≈ 12000 字节，字节级会砍到 1/3。
func TestBuildSystemPrompt_ChineseTruncation(t *testing.T) {
	// 每条 50 个汉字 = 50 rune；200 条 = 10000 rune > 4000
	var prof []pg.Memory
	for i := 0; i < 200; i++ {
		prof = append(prof, pg.Memory{Text: strings.Repeat("记", 50), Importance: 5})
	}
	out := buildSystemPrompt("BASE", prof, nil, nil, nil)
	// rune 计长：应包含约 4000/52 ≈ 76 条（每条 "- " + 50 字 + "\n" = 52 rune）
	// 字节计长只能装 4000/152 ≈ 26 条
	count := strings.Count(out, strings.Repeat("记", 50))
	if count < 60 || count > 90 {
		t.Fatalf("rune 截断应装 ~76 条，got %d", count)
	}
	if !strings.Contains(out, "截断") {
		t.Fatal("截断应有标记")
	}
	// 输出必须是合法 UTF-8（不能切出半个汉字）
	if !utf8.ValidString(out) {
		t.Fatal("截断产生了非法 UTF-8")
	}
}
