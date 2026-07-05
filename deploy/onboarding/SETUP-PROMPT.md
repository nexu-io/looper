# Prompt — 帮我配置并启动 looper(飞书 HITL)

把下面这段整段发给你的 coding agent(Claude Code / codex / …),在**解压后的这个目录里**运行它。

---

你在帮我配置并启动 **looper** —— 一个自主开发 agent 的守护进程 —— 以及它的「人在环(HITL)」飞书集成。**交互式**地陪我做:任何对外(GitHub / 飞书)或不可逆的操作前先跟我确认。

## 这个包里有什么
- `config.hitl.example.json` —— 带占位符的配置模板。
- `hitl.env` —— 团队**共享**的飞书 / Worker secret(已填好;**不要打印、不要写进 git、不要贴进聊天**)。
- `GUIDE-hitl-setup.md` —— 参考文档,先读它。

## 按这个来
1. **查前置条件**,缺什么告诉我:
   - `looperd` 和 `looper` 在 PATH 上(没有就问我路径 / 怎么装或编译)。
   - 我的 coding agent 二进制(`codex` 或 `claude`)已安装且**已登录/授权**(跑一下确认能用)。
   - 我要 looper 处理的那个 GitHub 仓库,本地已 clone。
2. **收集我的设置**(问我,别猜):
   - GitHub 仓库(`owner/repo`)+ 本地 clone 的**绝对路径**。
   - 我的飞书**群 chat id**(`oc_...`)和我的 **open_id**(`ou_...`)。我不知道就告诉我怎么拿(飞书开放平台调试台,或某条消息事件里的 `sender.open_id`)。
   - 我的 coding agent 二进制的绝对路径。
   - looper 数据/日志放哪(默认 `~/.looper`)。
3. **写我的配置**:把 `config.hitl.example.json` 复制到 `~/.looper/config.json`,把所有 `REPLACE_...` / `/ABSOLUTE/...` / `OWNER/REPO` 占位符替换成上面的值。改完把最终配置**给我看一遍**确认(它没有 secret,可以直接展示)。
4. **加载共享 secret**:`source` 这个包里的 `hitl.env`。**不要**把 secret 的值打印出来,也**不要**抄进配置文件(配置里只放变量名)。
5. **启动 looperd**:`source <包路径>/hitl.env && looperd --config ~/.looper/config.json`(后台跑,或按 `GUIDE-hitl-setup.md` 装成常驻守护进程)。确认活着:`looper --config ~/.looper/config.json status`。
6. **冒烟**(先问我):在我的仓库建一个带 `looper:plan` 标签的小 issue,确认 looper 接住、并在(有歧义时)往我的飞书群发一张决策卡 @我。

## 护栏
- 永远不要把 `hitl.env` 或配置提交进 git;永远不要把 secret 贴进聊天。
- 建 GitHub issue/PR、往飞书发东西之前**先问我**。
- 前置条件缺了就停下来告诉我,别假装装好了。
