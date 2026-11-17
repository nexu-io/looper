package agent

import "strings"

const (
	CompletionMarker       = "__LOOPER_RESULT__"
	CompletionMarkerPrefix = CompletionMarker + "="
)

func AppendCompletionInstruction(prompt string) string {
	return strings.Join([]string{
		prompt,
		"When finished, print exactly one final line to stdout in this format:",
		CompletionMarkerPrefix + `{"summary":"<one-sentence summary>"}`,
		"Do not wrap that line in markdown.",
		"Do not print anything after that line.",
	}, "\n\n")
}

// DeliveryInstruction keeps handoff requirements consistent across task executors.
const DeliveryInstruction = `交付任务前加载 task-delivery Skill（优先宿主已发现的 Skill；可读取 ~/.agents/skills/task-delivery/SKILL.md 或 ~/.codex/skills/task-delivery/SKILL.md）。它与 repo-find 配合，由 Agent 自主选择仓库及交付类型。缺失时明确报告缺失，继续按以下要求准备交付，不能声称已执行 Skill 检测。
依据原始需求提供可由验收人或其 Agent 复现的验收指南、准确版本、实际产物和检测报告；用项目真实检查验证成功路径、相关失败路径及能拒绝空实现/固定假结果的断言，并复核关键接线。文件非空、HTTP 200、编译或 CI 通过不代表功能满足需求。区分交付包检查、真实功能检测和人工验收，失败/阻塞/未执行如实记录，不发布成功交付通知；代码变更后重新验证。
共享预览和产物须为验收人可访问的 HTTPS，不能用 localhost 或本机路径代替；实际核对版本与访问权限。对于 Plane 来源任务，将指南、检测结果和产物关联原 Plane 任务并回读确认后再通知原飞书话题；飞书发送成功不代表 Plane 已归档。没有发布授权或关联失败时保留本地包并报告待归档，不自动创建替代任务、标 Done、合并或部署。`
