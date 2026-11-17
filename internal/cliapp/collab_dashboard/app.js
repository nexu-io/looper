let data = null;
let selectedTrace = null;
let tableSignature = "";
const $ = (s) => document.querySelector(s);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
const labels = {completed:"执行结束",awaiting_human:"等待人工",running:"执行中",queued:"排队中",failed:"失败",terminated:"已终止",paused:"暂停",success:"本轮结束",interrupted:"已中断",shepherding:"跟进 PR"};
const status = s => labels[s] || s || "未知";
const chip = s => `<span class="status-label ${Object.hasOwn(labels,s)?esc(s):""}">${esc(status(s))}</span>`;
const date = (v, full=false) => v ? new Intl.DateTimeFormat("zh-CN", {timeZone:"Asia/Shanghai",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",...(full?{second:"2-digit"}:{})}).format(new Date(v)) : "未记录";
const duration = ms => ms == null ? "未记录" : ms < 1000 ? `${ms} ms` : ms < 60000 ? `${(ms/1000).toFixed(1)} 秒` : `${Math.floor(ms/60000)} 分 ${Math.floor(ms%60000/1000)} 秒`;
const dayKey = d => new Intl.DateTimeFormat("en-CA",{timeZone:"Asia/Shanghai",year:"numeric",month:"2-digit",day:"2-digit"}).format(d);
async function get(path) {
  const r = await fetch(path, {cache:"no-store",signal:AbortSignal.timeout(12000)});
  if (!r.ok) throw new Error("data unavailable");
  return r.json();
}
function render() {
  if (!data) return;
  const filter = $("#status-filter").value;
  const tasks = data.tasks.filter(t => filter === "all" || t.status === filter);
  const completed = tasks.filter(t=>t.status==="completed").length;
  const waiting = tasks.filter(t=>t.status==="awaiting_human").length;
  const agents = tasks.reduce((s,t)=>s+t.agents,0);
  const tracked = tasks.filter(t=>t.coverage==="tracking_enabled").length;
  $("#metrics").innerHTML = [["业务任务",tasks.length,"按原任务计数，续跑不重复计单"],["执行结束",completed,"不代表人工验收、合并或发布"],["等待人工",waiting,"在原任务话题等待反馈"],["Agent 执行",agents,`${tasks.reduce((s,t)=>s+t.runs,0)} 次运行，包含续跑`]].map(([label,v,note])=>`<div class="metric"><div class="metric-label">${esc(label)}</div><div class="metric-value">${v}</div><div class="metric-note">${esc(note)}</div></div>`).join("");
  const days = Array.from({length:7},(_,i)=>{const d = new Date(Date.now()-(6-i)*86400000);const key=dayKey(d);return {key,n:tasks.filter(t=>dayKey(new Date(t.createdAt))===key).length};});
  const max = Math.max(1,...days.map(d=>d.n));
  $("#arrival-chart").innerHTML = days.map(d=>`<div class="day"><span class="count">${d.n||""}</span><div class="bar" style="height:${Math.max(2,d.n/max*100)}px"></div><span class="date">${esc(d.key.slice(5).replace("-","/"))}</span></div>`).join("");
  const stateCounts = ["completed","awaiting_human","running","queued","failed"].map(s=>({s,n:tasks.filter(t=>t.status===s).length}));
  $("#status-chart").innerHTML = stateCounts.map(d=>`<div class="status-row"><span><i class="dot ${d.s}"></i>${status(d.s)}</span><strong>${d.n}</strong></div>`).join("");
  $("#quality").innerHTML = [["有新追踪起点",`${tracked} / ${tasks.length}`],["业务验收事实","未接入"],["Token / 费用","未采集"]].map(([k,v])=>`<div class="quality-item"><strong>${esc(k)}</strong><span>${esc(v)}</span></div>`).join("");
  const signature = JSON.stringify(tasks);
  if (signature !== tableSignature) {
  tableSignature = signature;
  $("#task-table").innerHTML = tasks.map(t=>`<tr tabindex="0" data-seq="${t.seq}"><td class="task-name"><button class="task-open" aria-label="查看任务：${esc(t.title)}">${esc(t.title)}</button><div class="task-id">本机 #${t.seq} · ${esc(t.vendors.join(" / "))} · ${t.events} 条事件</div></td><td>${chip(t.status)}</td><td>${t.runs} / ${t.agents}</td><td>${duration(t.agentMs)}</td><td><span class="muted">${t.coverage==="tracking_enabled"?"新追踪已启用":"历史明细不全"}</span></td><td class="muted">${date(t.updatedAt)}</td></tr>`).join("") || '<tr><td colspan="6" class="detail-empty">当前筛选下没有任务。</td></tr>';
  $("#task-table").querySelectorAll("tr[data-seq]").forEach(row=>{row.onclick=()=>openTask(row.dataset.seq);row.onkeydown=e=>{if(e.key==="Enter")openTask(row.dataset.seq);};});
  }
  $("#updated").textContent = `更新于 ${date(data.asOf,true)}`;
}
const eventLabels={"task.received":"接收任务","plane.dispatch.received":"关联 Plane 派发","run.started":"开始一轮运行","plane.state.synced":"Plane 状态回写成功","task.state_changed":"任务状态变化","step.started":"阶段开始","step.completed":"阶段结束","agent.invoked":"启动 Agent","agent.completed":"Agent 执行结束","repository.reported":"Agent 上报仓库","artifacts.reported":"Agent 上报产物","run.ended":"本轮运行结束","human.requested":"等待人工反馈","human.answered":"接收人工回复","feishu.receipt":"消息收到回执","notification.delivered":"通知已投递","notification.sent":"通知记录"};
const stepLabels={"prepare-work":"准备任务","prepare-worktree":"准备任务目录","plan":"计划","execute":"执行","validate":"验证阶段","open-pr":"交付收尾","shepherd":"PR 跟进"};
async function openTask(seq) {
  $("#detail-title").textContent = data?.tasks.find(t=>String(t.seq)===String(seq))?.title || `任务 #${seq}`;
  $("#detail-body").textContent = "正在读取任务时间线…";
  $("#detail").showModal();
  try { selectedTrace=await get(`/api/tasks/${seq}`); renderTrace(selectedTrace); }
  catch { $("#detail-body").textContent="任务数据暂不可读，请关闭后重试。"; }
}
function renderTrace(d) {
  const reports=d.events.filter(e=>e.type==="repository.reported");
  const rows=(d.attempts||[]).map(r=>`<div class="run-row"><div>${chip(r.status)} <code>${esc(r.runId)}</code></div><span>${date(r.startedAt)} · ${duration(r.durationMs)} · ${(r.agents||[]).map(a=>esc(a.vendor)).join(" / ")||"无 Agent 记录"}</span></div>`).join("");
  const scopes=reports.length ? reports.map(e=>`<div class="run-row"><div>${esc(e.payload.repo)}<div class="muted">${esc(e.payload.role)} · source=agent_report</div></div><span class="muted">验证：${esc(e.payload.validationStatus||"未知")}${e.payload.prUrl?`<br>${esc(e.payload.prUrl)}`:""}</span></div>`).join("") : '<p class="detail-empty">没有结构化仓库上报；不据连接配置推断实现仓库。</p>';
  const timeline=d.events.map(e=>{const p=e.payload||{};let detail=p.step?stepLabels[p.step]||p.step:p.state?`回写 ${status(p.state)}`:p.from?`${status(p.from)} → ${status(p.to)}`:p.repo?p.repo:p.status?status(p.status):p.count?`${p.count} 个产物（Agent 上报）`:"";return `<div class="timeline-item"><time>${date(e.at,true)}</time>${esc(eventLabels[e.type]||e.type)}${detail?`<small>${esc(detail)}</small>`:""}</div>`;}).join("");
  $("#detail-body").innerHTML=`<div class="detail-summary">${chip(d.status)}<span>${d.coverage==="tracking_enabled"?"新追踪已启用":"历史追踪不完整"}</span><span>业务验收：未接入</span><span>费用：未知</span></div><p class="muted">${esc(d.traceId)}</p><div class="detail-section"><h3>运行与续跑</h3>${rows}</div><div class="detail-section"><h3>仓库与验证上报</h3>${scopes}</div><div class="detail-section"><h3>过程时间线 · ${d.events.length} 条事件</h3><div class="timeline">${timeline||'<p class="detail-empty">没有过程事件。</p>'}</div></div><div class="detail-actions"><button id="download-trace">下载筛选后的追踪 JSON</button></div>`;
  $("#download-trace").onclick=()=>{const a=document.createElement("a");const u=URL.createObjectURL(new Blob([JSON.stringify(d,null,2)],{type:"application/json"}));a.href=u;a.download=`looper-task-${d.seq}-trace.json`;a.click();setTimeout(()=>URL.revokeObjectURL(u),1000);};
}
async function refresh() {
  try {data=await get("/api/overview");$("#error").hidden=true;render();}
  catch {$("#error").hidden=false;$("#error").textContent="本机数据暂不可读。当前显示上次结果，请稍后刷新；页面没有启动任何任务。";}
}
$("#status-filter").onchange=render;
$("#refresh").onclick=refresh;
$("#close-detail").onclick=()=>$("#detail").close();
$("#detail").onclick=e=>{if(e.target===$("#detail"))$("#detail").close();};
refresh();setInterval(refresh,5000);
