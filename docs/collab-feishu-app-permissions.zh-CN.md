# Looper 飞书应用权限配置清单

正式应用：[Looper · cli_a936db5f74b89cee](https://open.feishu.cn/app/cli_a936db5f74b89cee/auth)。本清单用于将私测验证过的协作能力配置到正式应用；**记录不等于正式应用已开通或已验收**。

后台批量导入可用 [30 项 tenant 权限 JSON](collab-feishu-app-permissions.json)，覆盖消息协作、人员目录、文档及大文件交付；不含下文可选字段及 bridge 附加功能。先查看后台导入差异再发布，不删除正式应用已有权限。JSON 不包含事件订阅、可用范围或文档资源授权，这些仍需分别设置。

更新于 2026-11-18。对应测试版 `v0.16.3-beta.2`（源提交 `4546116`）、安装版任务 CF-16。权限来源包括固定版 bridge 的 `src/config/scopes.ts`、Looper 实际 API 调用、飞书 CLI schema 和真实错误。私测应用为 `cli_aaacdcfa88b91cfc`，与正式应用分开核对。

## 消息、卡片和任务群

下表均配置为**应用身份（tenant / bot）**。历史总权限 `im:chat`、`im:message` 可能覆盖部分调用；新应用优先使用下列细分权限，不把两套名字都当作必选。

| 权限 | 用途 | 私测验证情况 |
| --- | --- | --- |
| `im:message.group_at_msg:readonly` | 接收群内 @bot 消息 | 选择回复和原会话续跑通过 |
| `im:message.group_msg` | 读取所在群/话题上下文 | 话题回读通过；能读普通聊天不表示每条都响应 |
| `im:message.p2p_msg:readonly` | bridge 私聊消息入口 | 已授权，当前安装版验收只用私测群 |
| `im:message:readonly` | 回读消息、话题和消息附件 | bot 回读 HTML、图片通过 |
| `im:message:send_as_bot` | bot 发送消息、卡片、话题回复 | 通过，产品交付统一 bot 身份 |
| `im:message:update` | 更新已发送的任务/运行卡片 | 同卡状态更新通过 |
| `cardkit:card:write` | bridge 创建和更新 CardKit 卡片 | 流式运行卡通过 |
| `im:message.reactions:write_only` | 收到消息后添加/清理运行表情 | 真实选择消息的 Typing 回执已读回 |
| `im:message.reactions:read` | 查询回执、读取表情事件 | API 回读通过；表情事件交互需单独验证 |
| `im:resource` | 上传图片、HTML、报告、视频等消息资源 | 本轮 HTML/图片/指南/报告发送与 bot 下载通过 |
| `im:chat:create` | 为 module/需求创建协作群 | 本轮自动建群通过 |
| `im:chat:read` | 读取群信息和入口 | 已授权；上游还列历史名 `im:chat:readonly`，后台按实际可用项核对 |
| `im:chat:update` | 更新群名称、说明等 | 已授权；不以授权状态代替每项功能测试 |
| `im:chat.members:write_only` | 按需邀请同事入群 | 历史 bot 邀请罗高通过；本轮自动建群带 owner |
| `im:chat.members:read` | 邀请后确认实际成员，再 @ | 本轮已确认群中为 owner + bot |
| `im:chat.announcement:read` | 读取群公告内容/版本 | 历史真实读回通过 |
| `im:chat.announcement:write_only` | 写入 module/需求用途及 Plane 入口 | 历史真实写入通过 |
| `im:chat.top_notice:write_only` | 将公告显示在群顶部横幅 | 历史调用成功；私测旧总权限可覆盖，正式应用单独核对 |

`im:message.group_msg` 读取范围是 bot 所在会话，应用后台开通权限仍需发布生效。@bot 触发和普通聊天静默由 bridge 的对话策略处理，不靠撤掉上下文读取权限实现。

## 人员目录

| 权限 | 用途 | 私测验证情况 |
| --- | --- | --- |
| `contact:contact.base:readonly` | 读取应用授权的用户、部门和子部门目录 | 目录同步通过 |
| `contact:user.base:readonly` | 读取人员姓名，供 Agent 查阅找人 | 历史 46 人姓名读回通过；只拿到 ID 不能算人员目录完成 |
| `contact:user.employee:readonly` | 任职状态等可选字段 | 按需配置，缺字段表示未知，不阻塞普通协作 |

**应用可用范围及通讯录数据范围均需覆盖公司全员**，这是 owner 已确认的选择，使新同事进入范围后可被使用。只在版本里勾选几个个人不会覆盖未来入职同学。后台有用户组授权时，还要核对当前目录实现能否完整枚举，不能把部分结果宣称为全员。

不同应用的 `open_id` 不可复用。切换正式应用后重新同步目录、取得该应用对应的 owner/同事身份；Plane 人员与飞书人员关联不靠名字强制合并，不确定仍由 Agent 问 owner。

## 文档与验收产物

| 权限 | 用途 | 私测验证情况 |
| --- | --- | --- |
| `docx:document:create` | bot 创建验收说明文档 | 本轮创建成功，但不等于人能访问 |
| `docx:document:write_only` | 编辑文档正文/附件块 | 私测已授权，完整文档交付仍待复验 |
| `docx:document:readonly` | 回读文档正文及块 | 私测已授权，正式交付需实际回读 |
| `docx:document.block:convert` | 需要转换 Markdown/HTML 内容为文档块时使用 | 私测已授权，按实际 CLI 路径使用 |
| `docs:document.media:upload` | 将 HTML/图片等上传为文档附件资源 | **本轮 bot 未开通，附件上传未通过** |
| `docs:document.media:download` | 下载文档附件核对交付版本 | bot 尚未开通，文档附件回读待验证 |
| `docs:permission.member:create` | 给指定验收人/协作群授予文档访问权限 | **本轮 bot 未开通，CLI 创建后自动授权失败** |
| `docs:permission.member:retrieve` | 查询文档协作者，确认目标验收人在权限内 | bot 尚未开通，待验证 |

上述缺失项按真实 `scope_type=tenant` 查询确认；**同名 user scope 已开通不能替代 bot scope**。本轮 CLI 的 `auth scopes` 返回 user 列表，不能拿它证明 bot 已授权；核对采用 `GET /open-apis/application/v6/scopes` 并筛选 `scope_type=tenant`、`grant_status=1`。

CF-16 的文档创建成功，但 CLI 返回 `permission_grant` 失败，文档不是有效交付入口。按 owner 确定的方向，已降级到原话题的 **bot 原生 HTML 文件、截图、简短指南与检测报告**。bot 下载回读与源文件一致；验收 Agent 用 owner 身份只读下载 HTML/截图，HTML SHA-256 与源码一致。此读操作仅验证访问，不是 owner 真人 QA，也没有以 user 身份发送交付。

正式应用补齐后，仍需实测“bot 创建 → 写正文 → 附件上传 → 给目标人授权 → 目标人打开/下载 → 内容与交付版本一致”。权限齐全也不自动获得别人已有文档的访问权。不要为交付默认开启公网匿名访问或转移文档所有权。

### 超过消息附件上限的客户端或其他大文件

OPEND-5423 的约 417 MB 客户端无法作为消息附件发送：飞书 CLI 实际返回上限 100 MB。Plane 单请求上传也返回 Broken pipe，不能将本机下载按钮作为已共享入口。云盘分片上传是既有备选渠道，对应最小权限已补入上述 JSON：

| 权限 | 用途 | 私测验证情况 |
| --- | --- | --- |
| `drive:file:upload` | bot 将客户端等大文件上传云盘，可使用平台支持的分片接口 | 私测 bot 调用被拒绝；真实错误列此权限与 `drive:file`、`drive:drive` 为替代选项，优先申请细分上传权限 |
| `drive:file:download` | 下载云盘产物并核对交付版本 | 尚待开通及实测，不沿用消息附件下载的通过结果 |

上传后仍需用既有文档/文件授权能力给目标人访问，回读并由接收端下载核对；上传成功不等于他人可访问。不为了发送大包改用 user 身份。列目录/查文件如确实启用，另需 `drive:drive.metadata:readonly`，本次未列为必需。

缺少大文件共享权限时可以先发准确构建的原生截图/录屏，明确其证明范围，继续推进其它工作；这不能代替目标人下载并运行客户端的回归。

## bridge 附加功能：保留记录，按启用情况配置

这些来自固定版上游，不表示 Looper 当前全都启用，也不因缺少它们就添加新的启动门禁。

| 权限 | 对应功能 |
| --- | --- |
| `im:message.pins:write_only`、`im:message.pins:read` | 消息 Pin 的写入/回读，与群公告横幅不同 |
| `im:chat.managers:write_only` | 设置群管理员 |
| `im:chat.tabs:write_only` | 群标签页 |
| `im:chat.menu_tree:write_only` | 群内命令菜单 |
| `application:application.app_version:readonly` | 只读诊断已发布事件订阅 |
| `docs:document.comment:read`、`docs:document.comment:create` | 云文档评论接收与回复 |
| `wiki:wiki:readonly` | 将知识库入口解析为文档 token |

## 权限之外还需配置

1. 开启机器人能力，将所需 tenant 权限开通并发布应用版本；应用可用范围、通讯录范围为全员。
2. 事件订阅至少配置 `im.message.receive_v1`。接收表情事件时加 `im.message.reaction.created_v1`；卡片动作启用 `card.action.trigger` 回调。事件、回调不是权限 scope，不能只导入权限而漏配。
3. 多台本地 Looper 使用同一个应用时，沿现有共享 inbox 的请求地址与签名/加密配置接入，不能让每台机器争抢同一应用的长连接。回调实际支持范围按 [inbox 部署说明](../deploy/feishu-inbox-worker/README.md) 核对，不为补权限更换现有路由。
4. 先在 owner + bot 私测群验证自动建群、邀请后再 @、原 thread 续跑、表情/卡片、公告跳转、产物访问和 Plane 归档。正式应用自身的结果另记，不沿用私测应用的“通过”。

本文件是配置与复验清单，不增加程序化角色校验或任务门禁。缺少文档能力时继续选择可访问的附件/截图等交付形式；bot 权限不足不能靠切换成 user 发送产品消息来绕过。
