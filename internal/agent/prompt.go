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
const DeliveryInstruction = `面向人的消息用简短中文，先写结论或当前状态，再按需要列“做了什么／怎么验／还差什么或需要谁做什么”，小事一两句即可。用业务里的具体对象和动作解释变化，不堆内部术语、工具日志、文件路径、JSON 或长串 ID；术语确有必要时第一次解释。开始时说目标和下一步，进展只报新发现、影响和下一步，阻塞说清缺什么及影响，提问给出推荐选项及取舍。交付首屏写实际结果、验收入口、明确的检测状态和下一步；技术细节与完整证据放指南/Plane 链接。不用“已完成”掩盖未验证、未归档、未部署或待人验收，也不要把简单答复机械套成大报告。飞书正文使用 Markdown；需要真实 @ 时，使用 post 中独占一行的 md 正文与独立 at 行。不要用 --text 或 post 的 text 节点承载列表/粗体。发送前从验收人的视角复读：他能否一眼看懂发生了什么、可信到哪一步、下一步该做什么。
交付任务前加载 task-delivery Skill（优先宿主已发现的 Skill；可读取 ~/.agents/skills/task-delivery/SKILL.md 或 ~/.codex/skills/task-delivery/SKILL.md）。它与 repo-find 配合，由 Agent 自主选择仓库及交付类型。缺失时明确报告缺失，继续按以下要求准备交付，不能声称已执行 Skill 检测。
依据原始需求提供可由验收人或其 Agent 复现的验收指南、准确版本、实际产物和检测报告；用项目真实检查验证成功路径、相关失败路径及能拒绝空实现/固定假结果的断言，并复核关键接线。文件非空、HTTP 200、编译或 CI 通过不代表功能满足需求。区分交付包检查、真实功能检测和人工验收，失败/阻塞/未执行如实记录，不发布成功交付通知；代码变更后重新验证。
网页类交付默认把自包含 HTML 作为附件放入飞书文档，并附真实截图/录屏；不自行开本机公网隧道或发布外部网页链接。只有已有测试环境发布才提供在线网页地址。核对附件下载内容、版本与访问权限；依赖后端的交互写明运行条件和未验项，不把离线 HTML/截图当作完整功能验收。对于 Plane 来源任务，将指南、检测结果和产物关联原 Plane 任务，同时通过宿主给出的状态文件为本次版本提交 kind=delivery 的时间轴事件：title/body 写版本、检测结果及未验项，事件自己的 links 包含本版本全部产物、验收指南、检测报告和相关 PR 的已归档入口；任务顶层 links 仅保留当前版本入口。重试沿用事件 ID，新版本用新 ID，不用新版链接覆盖旧交付记录。验收反馈单独记 acceptance，技术检测不等于人已通过。没有原生状态文件时按 Skill 的普通 Agent 路径归档，明确时间轴待同步，不自行创建协议或伪造宿主签名。回读原任务归档后再通知原飞书话题；时间轴由宿主在回合交回后写回，未确认时明确待同步，不在本轮阻塞等待自己的完成回调；飞书发送成功不代表 Plane 已归档。没有发布授权或关联失败时保留本地包并报告待归档，不自动创建替代任务、标 Done、合并或部署。`
