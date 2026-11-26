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
交付按任务规模组织，以一份简短验收说明为主要入口，写清准确版本、如何复现、实际检测和剩余范围。小修复不为满足形式另外制造多篇报告、自检清单或完整源码包；已有 PR、页面和截图足够说明时直接复用，技术日志按需留档。依据原始需求提供可由验收人或其 Agent 复现的指南和实际产物；用项目真实检查验证成功路径、相关失败路径及能拒绝空实现/固定假结果的断言，并复核关键接线。文件非空、HTTP 200、编译或 CI 通过不代表功能满足需求。区分交付包检查、真实功能检测和人工验收，失败/阻塞/未执行如实记录，不发布成功交付通知；代码变更后重新验证。PR 的 CI 失败先对照准确 base/head 定位原因，不能仅因失败文件不在原 diff 中就搁置；在影响面和授权范围内继续修复，确需权限或人判断时提出具体问题，不跳过检查或把既有失败当通过。
已有网页项目的修复优先交付真实页面的截图/录屏和准确版本，不另造离线 HTML 副本作为规定产物；任务本身产出独立 HTML 时才按需作为附件共享。不自行开本机公网隧道或发布外部网页链接。只有已有测试环境发布才提供在线网页地址。给人的入口按接收人的正常登录方式核对下载内容、版本与访问权限；CLI 带 token 读回成功不代表浏览器能打开，不把仅接受机器凭据的 API 地址当验收入口。依赖后端的交互写明运行条件和未验项，不把离线 HTML/截图当作完整功能验收。对于 Plane 来源任务，将指南、检测结果和产物关联原 Plane 任务，同时通过宿主给出的状态文件为本次版本提交 kind=delivery 的时间轴事件：title/body 写版本、检测结果及未验项，任务顶层 links 和交付事件 links 放验收人现在要打开的少量主要入口（验收说明、关键效果/可用版本及相关 PR），不要把每份日志、清单、更正记录分别平铺；完整证据在同版验收说明的“详细证据”中用目录链接归档，旧版入口留在对应历史事件。重试沿用事件 ID，新版本用新 ID，不用新版链接覆盖旧交付记录。验收反馈单独记 acceptance，技术检测不等于人已通过。没有原生状态文件时按 Skill 的普通 Agent 路径归档，明确时间轴待同步，不自行创建协议或伪造宿主签名。回读原任务归档后再通知原飞书话题；时间轴由宿主在回合交回后写回，未确认时明确待同步，不在本轮阻塞等待自己的完成回调；飞书发送成功不代表 Plane 已归档。没有发布授权或关联失败时保留本地包并报告待归档，不自动创建替代任务、标 Done、合并或部署。`
