package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/voocel/ainovel-cli/internal/store"
)

// docsFileNames 是共创前会从设定文档目录读取的约定文件名（按稳定顺序）。
// 命名贴近用户直觉的中文文档名；任一缺失静默跳过，不报错。
var docsFileNames = []string{
	"小说总纲.md",
	"世界设定.md",
	"人物设定.md",
	"剧情规划.md",
	"写作规则.md",
}

// docsFileTitle 文件在注入提示里的小标题（去掉 .md 后缀）。
func docsFileTitle(name string) string {
	return strings.TrimSuffix(name, ".md")
}

// resolvedDocsDir 将 docsDir 解析为绝对路径，校验是否为目录。
// 返回空串表示无效或不存在。
func resolvedDocsDir(docsDir string) string {
	docsDir = strings.TrimSpace(docsDir)
	if docsDir == "" {
		return ""
	}
	if !filepath.IsAbs(docsDir) {
		if abs, err := filepath.Abs(docsDir); err == nil {
			docsDir = abs
		}
	}
	info, err := os.Stat(docsDir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return docsDir
}

// readDocsFromDir 读取 docsDir 下所有约定文件，返回文件名→内容的映射。
// 缺失文件跳过，不报错。
func readDocsFromDir(docsDir string) map[string]string {
	dir := resolvedDocsDir(docsDir)
	if dir == "" {
		return nil
	}
	docs := make(map[string]string)
	for _, name := range docsFileNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content != "" {
			docs[name] = content
		}
	}
	return docs
}

// buildDocsSummary 读取 docsDir 下约定的设定 md 文件，生成简短摘要注入共创系统提示。
// 与上一版的 buildDocsContext 不同：不再注入全文，只输出文档清单 + 每个文档的首段概览，
// 避免把大量设定文本塞进共创的 2048 token 窗口导致压缩和精度丢失。
// 实际的全文已通过 saveDocsToFoundation 存入 store，架构师后续通过 novel_context 读取。
//
// docsDir 为空 / 目录不存在 / 无任何约定文件 → 返回空串。
func buildDocsSummary(docsDir string) string {
	docs := readDocsFromDir(docsDir)
	if len(docs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n---\n## 用户提供的设定文档\n")
	b.WriteString("以下设定已从用户文档加载到创作基础中，可通过 novel_context 查看完整内容。\n")
	b.WriteString("共创时请在此基础上澄清与整理，不要忽略或推翻已写内容；未涉及的方面可补充。\n")

	for _, name := range docsFileNames {
		content, ok := docs[name]
		if !ok {
			continue
		}
		title := docsFileTitle(name)
		preview := extractDocPreview(content)
		b.WriteString("\n### ")
		b.WriteString(title)
		b.WriteString("（已加载）\n")
		if preview != "" {
			b.WriteString(preview)
			b.WriteString("\n")
		}
	}

	return b.String()
}

// extractDocPreview 从文档内容中提取简短概览（首段，最多 400 字）。
// 跳过 Markdown 标题行，取第一个实质性段落。
func extractDocPreview(content string) string {
	lines := strings.Split(content, "\n")
	var previewLines []string
	total := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(previewLines) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		runes := []rune(trimmed)
		if total+len(runes) > 400 {
			remaining := 400 - total
			if remaining > 0 {
				previewLines = append(previewLines, string(runes[:remaining])+"…")
			}
			break
		}
		previewLines = append(previewLines, trimmed)
		total += len(runes)
	}
	return strings.Join(previewLines, " ")
}

// saveDocsToFoundation 把用户设定文档存入 store，让架构师后续通过 novel_context 读取。
// 约定：
//   - 小说总纲.md → premise.md（直接映射，因为 premise 就是 Markdown 总纲）
//   - 其他四个文档 → meta/user_docs/xxx.md（纯文本存档，供架构师和编辑器参考）
//
// 任一文档缺失或为空均跳过，不报错。
// 写入失败会返回 error，调用方应记录日志但不中断共创流程。
func saveDocsToFoundation(docsDir string, s *store.Store) error {
	docs := readDocsFromDir(docsDir)
	if len(docs) == 0 {
		return nil
	}

	root := s.Dir()

	if content, ok := docs["小说总纲.md"]; ok {
		if err := s.Outline.SavePremise(content); err != nil {
			return fmt.Errorf("save premise from 小说总纲.md: %w", err)
		}
	}

	userDocsDir := filepath.Join(root, "meta", "user_docs")
	for _, name := range docsFileNames {
		if name == "小说总纲.md" {
			continue
		}
		content, ok := docs[name]
		if !ok {
			continue
		}
		if err := os.MkdirAll(userDocsDir, 0o755); err != nil {
			return fmt.Errorf("create user_docs dir: %w", err)
		}
		p := filepath.Join(userDocsDir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	return nil
}

// coCreateSystemPromptWithDocs 把冷启动共创系统提示与用户设定文档摘要拼接。
// 同时将文档全文存入 store，供架构师后续通过 novel_context 读取。
// docsDir 为空时等价于原 coCreateSystemPrompt，保持向后兼容。
func coCreateSystemPromptWithDocs(docsDir string, s *store.Store) string {
	if err := saveDocsToFoundation(docsDir, s); err != nil {
		return coCreateSystemPrompt + buildDocsSummary(docsDir)
	}
	return coCreateSystemPrompt + buildDocsSummary(docsDir)
}

// stageSystemPromptWithDocs 把阶段共创系统提示与用户设定文档摘要拼接。
// 阶段共创已有"当前故事状态"附录；设定文档摘要接在其后，两者不冲突。
// 同时将文档全文存入 store（阶段共创中 premise 可能已存在，SavePremise 会覆盖）。
func stageSystemPromptWithDocs(s *store.Store, docsDir string) string {
	if err := saveDocsToFoundation(docsDir, s); err != nil {
		return stageSystemPrompt(s) + buildDocsSummary(docsDir)
	}
	return stageSystemPrompt(s) + buildDocsSummary(docsDir)
}