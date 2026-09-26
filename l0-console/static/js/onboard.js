// ===================================================================
//  接入中心（Onboard Center）
//  职责：把「接入一个采集点」从一次性动作变成可观测的过程
//  - 总览：顶部统计 + 流水线列表（8 步圆点进度）
//  - 单条：纵向时间线，每步可展开看原始数据（配置快照/报错原文/拨测结果/指标样本）
//  - 新建：资源选择（手填 IP / 资源 ID）+ 模式 + 能力勾选 → 起流水线
//  数据来源：/api/onboard/*（模板、流水线、事件）
//  本模块自带样式，不依赖 style.css 追加（模块自包含，便于整块迁移）
// ===================================================================

(function () {
  if (window.__onboardCssLoaded) return;
  window.__onboardCssLoaded = true;
  var css = document.createElement('style');
  css.textContent = [
    '.ob-stat{display:flex;gap:12px;flex-wrap:wrap;margin-bottom:14px}',
    '.ob-stat .s{flex:1;min-width:120px;background:var(--card);border:1px solid var(--border);border-radius:8px;padding:10px 14px}',
    '.ob-stat .s .n{font-size:22px;font-weight:700;line-height:1.2}',
    '.ob-stat .s .l{font-size:11px;color:var(--muted)}',
    '.ob-dots{display:flex;gap:3px;align-items:center}',
    '.ob-dot{width:9px;height:9px;border-radius:50%;display:inline-block;box-sizing:border-box}',
    '.ob-dot.ok{background:var(--success)}',
    '.ob-dot.run{background:var(--warn)}',
    '.ob-dot.fail{background:var(--error)}',
    '.ob-dot.pend{background:transparent;border:1.5px solid var(--border)}',
    '.ob-tl{position:relative;padding-left:26px}',
    '.ob-tl:before{content:"";position:absolute;left:9px;top:6px;bottom:6px;width:2px;background:var(--border)}',
    '.ob-step{position:relative;margin-bottom:10px}',
    '.ob-step .ic{position:absolute;left:-26px;top:1px;width:20px;height:20px;border-radius:50%;background:var(--card);border:2px solid var(--border);display:flex;align-items:center;justify-content:center;font-size:11px;line-height:1;box-sizing:border-box}',
    '.ob-step.ok .ic{background:var(--success);border-color:var(--success);color:#fff}',
    '.ob-step.run .ic{background:var(--warn);border-color:var(--warn);color:#fff}',
    '.ob-step.fail .ic{background:var(--error);border-color:var(--error);color:#fff}',
    '.ob-step.blocked .ic{background:var(--error);border-color:var(--error);color:#fff}',
    '.ob-step .hd{font-size:13px;font-weight:600;display:flex;align-items:center;gap:8px;flex-wrap:wrap}',
    '.ob-step .meta{font-size:11px;color:var(--muted);margin-top:2px}',
    '.ob-step.pend .hd{color:var(--muted);font-weight:500}',
    '.ob-det{margin-top:8px;background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:8px 10px;font-size:11px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;word-break:break-all;max-height:280px;overflow:auto}',
    // 分级诊断条：探测/安装过程中的异常直接铺在步骤卡片上，不用点开才看得见
    '.ob-diag{margin-top:6px;padding:6px 9px;border-radius:5px;font-size:12px;line-height:1.5}',
    '.ob-diag.warn{background:#fef3c7;border:1px solid #fcd34d;color:#92400e}',
    '.ob-diag.fatal{background:#fee2e2;border:1px solid #fca5a5;color:#991b1b}',
    '.ob-diag.info{background:#e6f4f3;border:1px solid #adcfcd;color:#0b6e66}',
    '.ob-diag .hint{margin-top:2px;opacity:.85}',
    // 步骤结论条（2026-09-22 用户拍板）：没问题一句"通过"，有问题给关键信息+下一步动作
    '.ob-concl{margin:6px 0 2px;padding:7px 10px;border-radius:6px;font-size:12px;line-height:1.55}',
    '.ob-concl.ok{background:#ecfdf5;border:1px solid #6ee7b7;color:#065f46}',
    '.ob-concl.fail{background:#fee2e2;border:1px solid #fca5a5;color:#991b1b}',
    '.ob-concl.wait{background:#e6f4f3;border:1px solid #adcfcd;color:#0b6e66}',
    '.ob-concl.skip{background:#f3f4f6;border:1px solid #d1d5db;color:#4b5563}',
    '.ob-concl .why{margin-top:3px}',
    '.ob-concl .next{margin-top:3px;font-weight:600}',
    '.ob-concl .chips{display:block;margin-top:3px;opacity:.9}',
    '.ob-concl .code{margin-left:6px;font-size:10px;font-weight:400;opacity:.75;font-family:ui-monospace,Menlo,Consolas,monospace}',
    '.ob-next{margin:12px 0 4px;padding:9px 12px;border-radius:8px;font-size:12.5px;line-height:1.6;border:1px solid}',
    '.ob-next.ok{background:#e6f4f3;border-color:#adcfcd;color:#0b6e66}',
    '.ob-next.fail{background:#fee2e2;border-color:#fca5a5;color:#991b1b}',
    '.ob-next.wait{background:#f3f4f6;border-color:#d1d5db;color:#4b5563}',
    // 主机资源信息卡（反哺资源台账）：默认收起，点开看结构化档案
    '.ob-facts{margin-top:6px;border:1px dashed var(--border);border-radius:6px;background:var(--bg)}',
    '.ob-facts>.hd2{padding:6px 10px;font-size:12px}',
    '.ob-facts table{width:100%;border-collapse:collapse;font-size:11px}',
    '.ob-facts td{padding:3px 10px;border-top:1px solid var(--border);vertical-align:top;word-break:break-all}',
    '.ob-facts td:first-child{color:var(--muted);white-space:nowrap;width:92px}',
    '.ob-conn{margin-top:4px;font-size:11px;color:var(--muted)}',
    // 执行过程任务清单：安装/探路跑过的每个 TASK 都摊在卡片上（用户要求"所有信息都要显示出来"）
    '.ob-tasks{margin-top:6px;border:1px solid var(--border);border-radius:6px;overflow:hidden}',
    '.ob-tasks-hd{padding:5px 9px;font-size:11px;background:var(--bg);border-bottom:1px solid var(--border)}',
    '.ob-task{display:flex;gap:8px;align-items:baseline;padding:4px 9px;font-size:12px;border-bottom:1px solid var(--border)}',
    '.ob-task:last-child{border-bottom:0}',
    '.ob-task .idx{flex:0 0 20px;color:var(--muted);font-size:11px;text-align:right}',
    '.ob-task .st{flex:0 0 52px;font-size:11px;border-radius:3px;text-align:center}',
    '.ob-task .st.ok{color:var(--success)}',
    '.ob-task .st.changed{color:#0b6e66}',
    '.ob-task .st.failed,.ob-task .st.unreachable{color:var(--error);font-weight:600}',
    '.ob-task .st.skipping{color:var(--muted)}',
    '.ob-task .nm{flex:1;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-all}',
    '.ob-task .dur{flex:0 0 auto;color:var(--muted);font-size:11px}',
    '.ob-task.failed,.ob-task.unreachable{background:#fee2e2}',
    '.ob-task-sub{padding:2px 9px 4px 37px;font-size:11px;color:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-all}',
    '.ob-task-sub.err{color:#991b1b}',
    '.ob-timeout{margin-top:6px;padding:6px 9px;border-radius:5px;font-size:12px}',
    '.ob-timeout.run{background:#e6f4f3;border:1px solid #adcfcd;color:#0b6e66}',
    '.ob-timeout.over{background:#fee2e2;border:1px solid #fca5a5;color:#991b1b}',
    '.ob-attempts{margin-top:6px;font-size:11px;color:var(--muted)}',
    '.ob-attempts code{background:var(--bg);border-radius:3px;padding:0 4px}',
    '.ob-conn code{background:var(--bg);border-radius:3px;padding:1px 4px}',
    '.ob-warn{display:inline-block;background:#fef3c7;border:1px solid #fcd34d;color:#92400e;border-radius:8px;padding:0 6px;font-size:11px}',
    '.ob-row{cursor:pointer}',
    '.ob-row:hover{background:var(--bg)}',
    '.ob-empty{color:var(--muted);font-size:13px;padding:24px 0;text-align:center}',
    // 五阶段分组头（2026-09-21 评审）：11 步压成 5 段的阅读视图，引擎步骤粒度不变
    // 五环节块：一眼 5 块（2026-09-22 用户拍板「界面上就要 5 个环节」），
    // 步骤/诊断/超时/任务清单收进所属环节块内部，不再 10 卡平铺
    '.ob-sblock{border:1px solid var(--border);border-left:4px solid var(--muted);border-radius:8px;margin:12px 0;background:var(--card);overflow:hidden}',
    '.ob-sblock.ok{border-left-color:var(--success)}',
    '.ob-sblock.run{border-left-color:var(--warn)}',
    '.ob-sblock.fail{border-left-color:var(--error)}',
    '.ob-sblock>.hd{display:flex;align-items:baseline;gap:10px;flex-wrap:wrap;padding:10px 14px;cursor:pointer;user-select:none}',
    '.ob-sblock>.hd:hover{background:var(--bg)}',
    '.ob-sblock .no{font-weight:700;font-size:14px}',
    '.ob-sblock .nm{font-weight:700;font-size:14px}',
    '.ob-sblock .ex{color:var(--muted);font-size:11px}',
    '.ob-sblock .agg{margin-left:auto;font-size:11px;color:var(--muted);white-space:nowrap}',
    '.ob-sblock .pass{flex-basis:100%;color:var(--muted);font-size:11px}',
    '.ob-sblock>.bd{padding:6px 14px 12px 14px;border-top:1px dashed var(--border)}',
    // ===== 卸载向导操作台（2026-09-24 用户评审：从「详情卡」升级为「步骤式向导」）=====
    // 进入即一目了然：目标对象与已装插件 → 核心环节引导条 → 二次确认闸门 → 自动跑 + 实时日志 + 耗时 → 最终校验
    '.ob-wz{display:flex;flex-direction:column;gap:14px}',
    // ① 目标对象摘要 + 已装采集插件清单
    '.ob-wz-obj{border:1px solid var(--border);border-radius:10px;background:var(--card);padding:12px 14px;display:flex;gap:18px;flex-wrap:wrap;align-items:flex-start}',
    '.ob-wz-obj .who{min-width:180px}',
    '.ob-wz-obj .lbl{font-size:11px;color:var(--muted)}',
    '.ob-wz-obj .who b{font-size:15px;font-weight:600;display:block;line-height:1.4}',
    '.ob-wz-obj .who .ip{font-size:11px;color:var(--muted)}',
    '.ob-wz-obj .pl{flex:1;min-width:220px}',
    '.ob-wz-chips{display:flex;gap:6px;flex-wrap:wrap;margin-top:4px}',
    '.ob-wz-chip{display:inline-flex;align-items:center;gap:5px;font-size:11px;padding:3px 10px;border-radius:999px;background:var(--bg);border:1px solid var(--border)}',
    '.ob-wz-chip .g{width:7px;height:7px;border-radius:50%;background:var(--success);flex:0 0 auto}',
    '.ob-wz-chip.hot .g{background:var(--warn)}',
    '.ob-wz-chip .x{color:var(--muted)}',
    // ② 步骤引导条
    '.ob-wz-steps{display:flex;align-items:flex-start;border:1px solid var(--border);border-radius:10px;background:var(--card);padding:16px 14px}',
    '.ob-wz-st{flex:1;display:flex;flex-direction:column;align-items:center;gap:6px;min-width:0}',
    '.ob-wz-st .ring{width:26px;height:26px;border-radius:50%;display:flex;align-items:center;justify-content:center;font-size:12px;font-weight:700;border:2px solid var(--border);background:var(--card);color:var(--muted);box-sizing:border-box;flex:0 0 auto}',
    '.ob-wz-st .nm{font-size:11px;color:var(--muted);text-align:center;line-height:15px;word-break:keep-all}',
    // 段内进度 n/m：等宽字体，一眼看清这一段还剩几步
    '.ob-wz-st .sub{font-size:10px;color:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}',
    '.ob-wz-st.ok .sub{color:var(--success)}',
    '.ob-wz-st.fail .sub{color:var(--error)}',
    '.ob-wz-st.ok .ring{background:var(--success);border-color:var(--success);color:#fff}',
    '.ob-wz-st.ok .nm{color:var(--text);font-weight:600}',
    '.ob-wz-st.run .ring{border-color:var(--primary);color:var(--primary);box-shadow:0 0 0 4px rgba(8,125,117,.12);animation:obWzPulse 1.4s ease-in-out infinite}',
    '.ob-wz-st.run .nm{color:var(--primary);font-weight:600}',
    '.ob-wz-st.fail .ring{background:var(--error);border-color:var(--error);color:#fff}',
    '.ob-wz-st.fail .nm{color:var(--error);font-weight:600}',
    '.ob-wz-st.wait .ring{border-color:var(--warn);color:var(--warn);background:#fffbeb}',
    '.ob-wz-st.wait .nm{color:var(--warn);font-weight:600}',
    '.ob-wz-st.skip .ring{border-style:dashed;color:var(--muted)}',
    '@keyframes obWzPulse{0%,100%{box-shadow:0 0 0 3px rgba(8,125,117,.10)}50%{box-shadow:0 0 0 7px rgba(8,125,117,.05)}}',
    '.ob-wz-lk{flex:0 0 22px;height:2px;background:var(--border);margin-top:12px;border-radius:2px}',
    '.ob-wz-lk.on{background:var(--success)}',
    // ③ 结论横幅
    '.ob-wz-concl{padding:12px 14px;border-radius:10px;border:1px solid;display:flex;gap:10px;align-items:flex-start}',
    '.ob-wz-concl .dot{width:20px;height:20px;border-radius:50%;display:flex;align-items:center;justify-content:center;font-size:12px;flex:0 0 auto;color:#fff;margin-top:1px}',
    '.ob-wz-concl .tt{font-size:13px;font-weight:600;line-height:1.5}',
    '.ob-wz-concl .sub{font-size:12px;line-height:1.6;margin-top:2px;opacity:.9}',
    '.ob-wz-concl.run{background:#e6f4f3;border-color:#adcfcd;color:#0b6e66}',
    '.ob-wz-concl.run .dot{background:var(--primary)}',
    '.ob-wz-concl.ok{background:#ecfdf5;border-color:#6ee7b7;color:#065f46}',
    '.ob-wz-concl.ok .dot{background:var(--success)}',
    '.ob-wz-concl.fail{background:#fee2e2;border-color:#fca5a5;color:#991b1b}',
    '.ob-wz-concl.fail .dot{background:var(--error)}',
    '.ob-wz-concl.wait{background:#fffbeb;border-color:#fcd34d;color:#92400e}',
    '.ob-wz-concl.wait .dot{background:var(--warn)}',
    // ④ 两栏（确认卡 / 耗时表）
    '.ob-wz-grid{display:grid;grid-template-columns:1.35fr 1fr;gap:14px}',
    '@media(max-width:900px){.ob-wz-grid{grid-template-columns:1fr}}',
    '.ob-wz-card{border:1px solid var(--border);border-radius:10px;background:var(--card);padding:12px 14px}',
    '.ob-wz-card .chd{display:flex;align-items:center;gap:8px;margin-bottom:10px;font-size:12px;font-weight:600}',
    '.ob-wz-card .chd .r{margin-left:auto;font-weight:400;font-size:11px;color:var(--muted)}',
    // 耗时表
    '.ob-wz-dtab{width:100%;border-collapse:collapse;font-size:12px}',
    '.ob-wz-dtab td{padding:6px 6px;border-bottom:1px solid var(--border)}',
    '.ob-wz-dtab tr:last-child td{border-bottom:0}',
    '.ob-wz-dtab .sn{color:var(--muted);width:20px}',
    '.ob-wz-dtab .du{text-align:right;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted);white-space:nowrap}',
    '.ob-wz-dtab tr.ok .du{color:var(--success)}',
    '.ob-wz-dtab tr.run .du{color:var(--primary)}',
    '.ob-wz-dtab tr.fail .du{color:var(--error)}',
    '.ob-wz-dtab tr.run .sn{color:var(--primary);font-weight:700}',
    '.ob-wz-dtab tr.fail .sn{color:var(--error);font-weight:700}',
    '.ob-wz-dtab tr.tot td{border-top:2px solid var(--border);font-weight:600;padding-top:8px}',
    '.ob-wz-dtab tr.tot .du{color:var(--text)}',
    // ⑤ 实时日志终端
    '.ob-wz-term{border-radius:10px;overflow:hidden;border:1px solid #0a1220;background:#0d1526}',
    '.ob-wz-term .bar{display:flex;align-items:center;gap:6px;padding:7px 12px;background:#131a2e}',
    '.ob-wz-term .dots{display:flex;gap:6px}',
    '.ob-wz-term .d{width:10px;height:10px;border-radius:50%}',
    '.ob-wz-term .d1{background:#ff5f57}.ob-wz-term .d2{background:#febc2e}.ob-wz-term .d3{background:#28c840}',
    '.ob-wz-term .tt{color:#5d759f;font-size:11px;margin-left:8px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}',
    '.ob-wz-term .bd{padding:12px 14px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;line-height:1.75;max-height:320px;overflow:auto}',
    '.ob-wz-ln{display:flex;gap:8px;align-items:baseline}',
    '.ob-wz-ln .ts{color:#5d759f;flex:0 0 auto}',
    '.ob-wz-ln .tx{color:#9fb4d8;word-break:break-all;white-space:pre-wrap}',
    '.ob-wz-ln.ok .tx{color:#4ade80}',
    '.ob-wz-ln.run .tx{color:#67e8f9}',
    '.ob-wz-ln.fail{background:rgba(248,113,113,.10);border-radius:4px;padding:1px 4px}',
    '.ob-wz-ln.fail .tx{color:#f87171}',
    '.ob-wz-ln.warn .tx{color:#fbbf24}',
    '.ob-wz-ln .pos{color:#f87171;text-decoration:underline;cursor:pointer}',
    '.ob-wz-ln .det{color:#5d759f}',
    // 待复核提示：从执行器长串里拆出来单独着色，别埋在一堆 = 号里
    '.ob-wz-ln .ob-wz-warn{color:#fbbf24;font-weight:600}',
    '.ob-wz-cur{display:inline-block;width:8px;height:13px;background:#67e8f9;vertical-align:-2px;animation:obWzBlink 1s steps(2) infinite}',
    '@keyframes obWzBlink{50%{opacity:0}}',
    // ⑥ 最终卸载校验
    '.ob-wz-check{border:1px solid var(--border);border-radius:10px;background:var(--card);padding:12px 14px}',
    '.ob-wz-check.pass{border-color:#6ee7b7;background:#f0fdf7}',
    '.ob-wz-check .hd{display:flex;align-items:center;gap:8px;font-size:12px;font-weight:600;margin-bottom:8px}',
    '.ob-wz-check .hd .r{margin-left:auto;font-size:11px;font-weight:600}',
    '.ob-wz-check.pass .hd .r{color:var(--success)}',
    '.ob-wz-ck{display:flex;align-items:center;gap:8px;font-size:12px;padding:6px 8px;border-radius:6px;background:var(--bg);margin-bottom:5px}',
    '.ob-wz-ck:last-child{margin-bottom:0}',
    '.ob-wz-ck .st{flex:0 0 16px;height:16px;border-radius:50%;display:flex;align-items:center;justify-content:center;font-size:10px;color:#fff;background:var(--muted)}',
    '.ob-wz-ck.ok .st{background:var(--success)}',
    '.ob-wz-ck.bad .st{background:var(--error)}',
    '.ob-wz-ck .nm{font-weight:600;flex:0 0 auto}',
    '.ob-wz-ck .vv{color:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11px;margin-left:auto;text-align:right;word-break:break-all}',
    // ⑦ 步骤详情（折叠，信息零删减）
    '.ob-wz-acc{margin-top:2px}',
    '.ob-wz-acc>.h{display:flex;align-items:center;gap:8px;padding:8px 12px;cursor:pointer;font-size:12px;border:1px solid var(--border);border-radius:8px;background:var(--card)}',
    '.ob-wz-acc>.h:hover{background:var(--bg)}',
    '.ob-wz-acc>.h .ar{margin-left:auto;color:var(--muted);font-size:11px}',
    '.ob-wz-acc>.b{padding:10px 4px 2px 4px}',
    // 自动刷新指示：让运维知道界面自己在拉数据，不用手点「刷新」
    '.ob-flow-live{display:inline-flex;align-items:center;gap:6px;font-size:11px;color:var(--muted)}',
    '.ob-flow-live .dot{width:6px;height:6px;border-radius:50%;background:var(--success);animation:obLivePulse 1.6s ease-in-out infinite}',
    '@keyframes obLivePulse{0%,100%{opacity:1}50%{opacity:.2}}',
  ].join('');
  document.head.appendChild(css);
})();

// ---- 状态元数据（一处定义，列表/时间线共用）----
var OB_STEP_ICON = { pending: '○', running: '◐', ok: '✓', fail: '✕', blocked: '!', skipped: '–' };
var OB_BADGE = { running: 'b-s', done: 'b-h', failed: 'b-o', blocked: 'b-o', stalled: 'b-o', canceled: 'b-r' };
var OB_LABEL = { running: '接入中', done: '已完成', failed: '失败', blocked: '被阻断', stalled: '卡住', canceled: '已取消' };

// ---- 四段视图（2026-09-24 用户拍板：接入流程按「安装接入」自己的语义定义，
//      卸载那边只借版面骨架与视觉，不借流程定义）----
// 引擎步骤保持原粒度不动（每个停等/门禁环节独立留痕、独立可重试），这里只做展示层归组。
// 归组依据是「接入这件事的四个里程碑」，不是把步骤平均分堆：
//   ① 声明需求：人先说清"采哪台、采什么、怎么连"——纯意图输入，平台不做判断
//   ② 探路定案：平台实测主机事实与采集可达性 → 按兼容矩阵定版本（全程唯一的人工决策点）
//   ③ 装机注册：分发（就近取包 + sha256 校验）→ 写配置 → 启动 → 注册回平台（心跳 = 存活的唯一证据）
//   ④ 验证入库：配置生效 → 探针真连取回一条指标 → 样本进 VM（"下发成功"不等于"真能采到"）
// 不在组内的步骤（offboard/upgrade/service 等）照旧单独渲染。
var OB_STAGES = [
  { no: '①', name: '声明需求', executor: '人',
    steps: ['pick_object', 'pick_proxy', 'pick_ability', 'collect_params'],
    pass: '选定被采对象与承载机 → 声明要采什么 → 按插件 params.yaml 落参数并建采集目标' },
  { no: '②', name: '探路定案', executor: '平台实测 + 人定版',
    steps: ['preflight_host', 'preflight_remote', 'pick_version'],
    pass: '实测 OS/架构/内核/磁盘/端口/是否已有 SAgent + 按所选能力核对可行性 → 从代理机侧核对被采目标可达性 → 人工选定版本（留痕审计）' },
  { no: '③', name: '装机注册', executor: '平台 + Agent',
    steps: ['install_agent', 'register_platform_device', 'self_metrics'],
    pass: '就近取包（L1 缓存 → 回源 L0，sha256 校验）→ 写配置 → 启动 → 注册回平台（Agent 发出心跳，主机自监控生效）' },
  { no: '④', name: '验证入库', executor: '平台 + Agent',
    steps: ['sync_config', 'verify_probe', 'observe_collect'],
    pass: '期望配置下发并生效（版本 +1）· 探针真连取回一条指标 · 样本进入 VictoriaMetrics' }
];

function obStageIndex(stepID) {
  for (var i = 0; i < OB_STAGES.length; i++) {
    if (OB_STAGES[i].steps.indexOf(stepID) >= 0) return i;
  }
  return -1;
}

// 组内状态聚合：fail/blocked > running > pending > ok（fail 优先示警，办结需全绿）；
// skipped（不适用跳过，如无需参数）算办结——阶段头要反映「这一段没有未决事项」
// 分母只算「本模板真的有这一步」的：四段是跨模式共用的归组表，edge 没有
// pick_proxy/preflight_remote/register_platform_device——把它们算进分母，
// ① 段永远差 2 步，永远显示不出「已办结」
function obStageAgg(stage, stMap) {
  var okN = 0, run = false, bad = false, total = 0;
  for (var i = 0; i < stage.steps.length; i++) {
    if (!stMap || !(stage.steps[i] in stMap)) continue;
    total++;
    var st = stMap[stage.steps[i]] || 'pending';
    if (st === 'fail' || st === 'blocked') bad = true;
    else if (st === 'running') run = true;
    else if (st === 'ok' || st === 'skipped') okN++;
  }
  var cls = bad ? 'fail' : (run ? 'run' : (total > 0 && okN === total ? 'ok' : ''));
  var label = bad ? '受阻' : (run ? '进行中' : (total > 0 && okN === total ? '已办结' : '待开始'));
  return { cls: cls, label: label, done: okN, total: total };
}

// obStageToggle 环节块折叠/展开（含箭头翻转，箭头始终表示当前状态）
function obStageToggle(id) {
  var bd = document.getElementById(id), ar = document.getElementById(id + '-ar');
  if (!bd) return;
  var open = bd.style.display !== 'none';
  bd.style.display = open ? 'none' : 'block';
  if (ar) ar.textContent = open ? '▼ 展开' : '▲ 收起';
}

// obStageBlock 渲染一个环节块：块头（编号+名称+执行者+进度+通过条件）+ 块体（组内步骤卡片）。
// 已办结的环节默认收起（点块头展开），进行中/受阻的默认展开——运维进门先看到没办完的事。
// steps 为空的伪阶段（「其他步骤」桶，卸载/离线流程等不在五环节内的步骤）永远展开，
// 聚合文案不用 n/n（组外步骤没有通过条件语义），直接报步数
function obStageBlockStMap(stage, evIdxs, evs, f, stMap) {
  var a = obStageAgg(stage, stMap);
  var id = 'ob-sb-' + stage.no;
  var expanded, aggTxt, cls = a.cls;
  if (!stage.steps.length) {
    // 「其他步骤」桶（卸载/离线流程等）：没有预定义步骤清单，状态按桶内事件实算，
    // 不走 obStageAgg（steps 为空会被它聚合成 0/0 已办结的假绿色）
    expanded = true; aggTxt = evIdxs.length + ' 步';
    cls = ''; var anyRun = false, anyBad = false;
    for (var q = 0; q < evIdxs.length; q++) {
      var qs = evs[evIdxs[q]].status || 'pending';
      if (qs === 'fail' || qs === 'blocked') anyBad = true;
      else if (qs === 'running') anyRun = true;
    }
    cls = anyBad ? 'fail' : (anyRun ? 'run' : '');
  } else {
    expanded = a.cls !== 'ok'; // 已办结收起，其余展开
    aggTxt = a.label + ' ' + a.done + '/' + a.total;
  }
  var h = '<div class="ob-sblock ' + cls + '">';
  h += '<div class="hd" onclick="obStageToggle(\'' + id + '\')">';
  h += '<span class="no">' + obEscape(stage.no) + '</span>';
  h += '<span class="nm">' + obEscape(stage.name) + '</span>';
  if (stage.executor) h += '<span class="ex">' + obEscape(stage.executor) + '</span>';
  h += '<span class="agg">' + obEscape(aggTxt) +
    ' <span id="' + id + '-ar">' + (expanded ? '▲ 收起' : '▼ 展开') + '</span></span>';
  h += '<span class="pass">通过条件：' + obEscape(stage.pass) + '</span>';
  h += '</div>';
  h += '<div class="bd ob-tl" id="' + id + '" style="display:' + (expanded ? 'block' : 'none') + '">';
  // 不适用的环节不出现在流程里（图 4）：状态为 skipped 且没有诊断的步骤不占版面，
  // 收成块尾一行"未执行"折叠——审计要能回溯，但日常视图不该被"已跳过"刷屏
  var skippedIdx = [];
  for (var i = 0; i < evIdxs.length; i++) {
    var e0 = evs[evIdxs[i]], st0 = e0.status || 'pending';
    var dd0 = obDetail(e0);
    var hasIssue = dd0 && dd0.diagnosis && dd0.diagnosis.length > 0;
    if (st0 === 'skipped' && !hasIssue) { skippedIdx.push(evIdxs[i]); continue; }
    h += obStepCard(f, e0, evIdxs[i]);
  }
  if (skippedIdx.length) {
    var sid = 'ob-skp-' + stage.no;
    var names = skippedIdx.map(function (ix) { return evs[ix].title || evs[ix].step; });
    h += '<div style="margin:6px 0"><button class="btn btn-o btn-sm" onclick="obToggleAny(\'' + sid + '\')">'
      + skippedIdx.length + ' 个环节不适用本次接入（未执行）：' + obEscape(names.join('、')) + '</button></div>';
    h += '<div id="' + sid + '" style="display:none">';
    for (var k2 = 0; k2 < skippedIdx.length; k2++) h += obStepCard(f, evs[skippedIdx[k2]], skippedIdx[k2]);
    h += '</div>';
  }
  h += '</div></div>';
  return h;
}

// obNextStepBar 全流程唯一的"我该做什么"（图 4）：
// 无论什么状态都给一条明确出口——完成了说"无需操作"，失败了说先处理哪一步，
// 停等说去下方决策卡确认，自动执行中说"等就行"。永远只有一条，不并列。
function obNextStepBar(f, evs) {
  var st = f.status || '';
  var cls = 'ok', icon = '→', text = '';
  var firstFail = null;
  for (var i = 0; i < evs.length; i++) {
    var s = evs[i].status || 'pending';
    if ((s === 'fail' || s === 'blocked') && !firstFail) firstFail = evs[i];
  }
  if (st === 'done') {
    text = '<b>无需操作</b>——接入已完成，数据已在采集。采集实况见「资源与指标」页对应主机。';
  } else if (st === 'canceled') {
    cls = 'wait'; icon = '⏹';
    text = '<b>流水线已取消</b>——如需重新接入，在资源页对该主机点「接入」。';
  } else if (st === 'failed') {
    cls = 'fail'; icon = '✕';
    var nm = firstFail ? obEscape(firstFail.title || firstFail.step) : '失败环节';
    text = '先处理失败环节「' + nm + '」——在该环节卡片内点「↻ 重试该步」重新执行；'
      + '确属环境/凭据问题就先修好再重试（修复入口就在该环节卡片里）。';
  } else if (st === 'blocked' || (st === 'running' && f.wait_kind === 'human')) {
    cls = 'wait'; icon = '⏸';
    text = '需要你确认：<b>' + obEscape(f.waiting_label || f.current_title || '待人工确认') + '</b>——决策卡在下方对应环节内，确认后流程自动继续。';
  } else if (st === 'running') {
    if (f.wait_kind === 'external') {
      text = '<b>无需操作</b>——平台正在「' + obEscape(f.current_title || f.current_step || '执行') + '」，完成后自动回报并继续；点「↻ 刷新」看最新进展。';
    } else {
      text = '<b>无需操作</b>——「' + obEscape(f.current_title || f.current_step || '执行中') + '」执行中，平台会自动推进。';
    }
  } else if (st === 'pending') {
    text = '流水线待启动——平台即将开始「' + obEscape(f.current_title || f.current_step || '第一步') + '」。';
  } else {
    text = '流水线状态：' + obEscape(st) + '。';
  }
  return '<div class="ob-next ' + cls + '">' + icon + ' <b>下一步：</b>' + text + '</div>';
}

// 列表页进度点：归入五阶段的模板 1 阶段 1 个点（title 展开组内明细）；
// 没有归进组的步骤（offboard 等）仍按步骤逐个出点
function obStageDots(stepIDs, st) {
  stepIDs = stepIDs || [];
  st = st || {};
  var h = '<div class="ob-dots">', grouped = {}, anyStage = false;
  for (var s = 0; s < OB_STAGES.length; s++) {
    var g = OB_STAGES[s], present = false, bad = false, run = false, okN = 0, parts = [];
    for (var i = 0; i < g.steps.length; i++) {
      if (stepIDs.indexOf(g.steps[i]) < 0) continue;
      present = true; grouped[g.steps[i]] = true;
      var stt = st[g.steps[i]] || 'pending';
      if (stt === 'fail' || stt === 'blocked') bad = true;
      else if (stt === 'running') run = true;
      else if (stt === 'ok' || stt === 'skipped') okN++;
      parts.push(g.steps[i] + '·' + stt);
    }
    if (!present) continue;
    anyStage = true;
    // 分母用 parts.length（本模板真有的步数），不是 g.steps.length——同 obStageAgg
    var cls = bad ? 'fail' : (run ? 'run' : (okN === parts.length ? 'ok' : 'pend'));
    h += '<span class="ob-dot ' + cls + '" title="' + obEscape(g.no + ' ' + g.name + ' ' + okN + '/' + parts.length + '（' + parts.join('，') + '）') + '"></span>';
  }
  for (var j = 0; j < stepIDs.length; j++) {
    if (grouped[stepIDs[j]]) continue;
    var s2 = st[stepIDs[j]] || 'pending';
    var c2 = s2 === 'ok' ? 'ok' : (s2 === 'running' ? 'run' : (s2 === 'fail' || s2 === 'blocked' ? 'fail' : 'pend'));
    h += '<span class="ob-dot ' + c2 + '" title="' + obEscape(stepIDs[j] + ' · ' + s2) + '"></span>';
  }
  return h + '</div>' + (anyStage ? '' : '');
}

// 等人工决策 ≠ 被阻断：blocked 只说明「依赖没满足」，
// 停等人工（wait_kind=human）时文案与配色都要软下来，否则运维会被吓到去排查一个不存在的故障
function obBadge(status, waitKind) {
  if (status === 'blocked' && waitKind === 'human') {
    return '<span class="badge b-s">待人工决策</span>';
  }
  return '<span class="badge ' + (OB_BADGE[status] || 'b-r') + '">' + (OB_LABEL[status] || status || '-') + '</span>';
}

function obEscape(s) {
  return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// ===================================================================
//  页面：接入中心
// ===================================================================
function renderOnboardCenter() {
  var el = document.getElementById('main-content');
  el.innerHTML = '<div class="card"><div class="card-bd"><div class="ob-empty">加载接入流水线…</div></div></div>';

  fetch(API + '/onboard/flows').then(function (r) { return r.json(); }).then(function (d) {
    var flows = (d && d.flows) || [];
    var sum = (d && d.summary) || {};
    var html = '';

    // —— 顶部统计 ——
    html += '<div class="ob-stat">';
    html += '<div class="s"><div class="n" style="color:var(--warn)">' + (sum.running || 0) + '</div><div class="l">接入中</div></div>';
    html += '<div class="s"><div class="n" style="color:var(--success)">' + (sum.done || 0) + '</div><div class="l">已完成</div></div>';
    html += '<div class="s"><div class="n" style="color:var(--error)">' + (sum.failed || 0) + '</div><div class="l">失败</div></div>';
    html += '<div class="s"><div class="n" style="color:var(--error)">' + (sum.stalled || 0) + '</div><div class="l">卡住</div></div>';
    html += '<div class="s"><div class="n">' + (sum.total || 0) + '</div><div class="l">全部</div></div>';
    html += '</div>';

    // —— 流水线列表 ——
    html += '<div class="card"><div class="card-hd"><span>🧭 接入流水线</span>';
    html += '<button class="btn btn-p btn-sm" style="margin-left:auto" onclick="openOnboardNew()">➕ 新建接入</button></div><div class="card-bd">';

    if (!flows.length) {
      html += '<div class="ob-empty">还没有接入流水线。<br>点击右上角「➕ 新建接入」，从一台主机或一个数据库实例开始。</div>';
    } else {
      html += '<table><thead><tr><th style="width:180px">资源</th><th style="width:90px">模式</th><th style="width:220px">采集能力</th><th style="width:150px">进度</th><th style="width:110px">状态</th><th style="width:200px">当前环节</th><th style="width:150px"></th></tr></thead><tbody>';
      for (var i = 0; i < flows.length; i++) {
        var f = flows[i];
        var abil = (f.abilities || []).join(', ') || '—';
        // 模式列显示人话（"边缘采集"/"卸载 SAgent"）；卸载流水线加图标与标识，
        // 一眼能和接入流水线区分开（同一个列表里并存，靠 id 猜是不行的）
        var modeCell = obEscape(f.mode_name || f.mode);
        if (f.is_offboard) modeCell = '<span title="卸载流水线（既有接入的逆操作）">🗑 ' + modeCell + '</span>';
        else if (f.mode === 'upgrade') modeCell = '<span title="升级流水线（既有接入的运维操作）">⬆ ' + modeCell + '</span>';
        else if (f.mode === 'service') modeCell = '<span title="启停流水线（既有接入的运维操作）">⏻ ' + modeCell + '</span>';
        // 维护态徽标（2026-09-22 用户拍板）：Agent 已停 → 心跳消失是预期，不算失联
        if (f.svc_state === 'stopped') modeCell += ' <span class="ob-warn" style="color:#854f0b" title="Agent 已停止（维护）——心跳消失是预期表现，不算失联">⏸ 已停止（维护）</span>';
        html += '<tr class="ob-row" onclick="openOnboardFlow(' + f.id + ')">';
        html += '<td><b>' + obEscape(f.resource_id) + '</b><div style="font-size:11px;color:var(--muted)">' + obEscape(f.resource_ip || f.agent_id || '') + '</div></td>';
        html += '<td>' + modeCell + '</td>';
        html += '<td style="font-size:12px">' + obEscape(abil) + '</td>';
        html += '<td>' + obStageDots(f.step_ids || [], f.step_status || {}) + '</td>';
        html += '<td>' + obBadge(f.status, f.wait_kind) + '</td>';
        html += '<td style="font-size:12px">' + obEscape(f.current_title || f.current_step || '—');
        if (f.status === 'running' && f.stall_sec) html += ' <span style="color:' + (f.stalled ? 'var(--error)' : 'var(--muted)') + '">已等待 ' + f.stall_sec + 's</span>';
        // 探测/安装过程中的待复核项（unknown 类未核实告警）：不点进去也要能看见
        if (f.warn_count) html += ' <span class="ob-warn" title="探测/安装过程中发现 ' + f.warn_count + ' 项待复核（不阻断流程，但需要人工确认）">⚠ ' + f.warn_count + ' 项待复核</span>';
        // 超时预算：不点进去也要知道"这一步还会等多久、超了会怎样"
        if (f.timeout_view) html += obTimeoutBadge(f.timeout_view);
        html += '</td>';
        html += '<td onclick="event.stopPropagation()" style="white-space:nowrap"><button class="btn btn-o btn-sm" onclick="openOnboardFlow(' + f.id + ')">查看过程</button>';
        // 卸载入口：只对"真的装过"的接入流水线出现（agent 台账有登记，或安装环节已 ok/skipped）。
        // 卸载完成后 agent 记录被注销，按钮自动消失——语义由台账驱动，不靠前端记忆
        if (f.can_offboard) {
          html += ' <button class="btn btn-d btn-sm" title="卸载该主机上的 SAgent 并清空平台登记" onclick="obOffboard(' + f.id + ')">卸载</button>';
        }
        // 升级/启停入口（2026-09-22）：同一台账驱动语义——Agent 登记着才给按钮；
        // 运维流水线自身不嵌套同类入口（后端 can_upgrade/can_service 已收敛）
        if (f.can_upgrade) {
          html += ' <button class="btn btn-o btn-sm" title="升级该主机上的 SAgent 到指定版本（允许降级，留痕）" onclick="obUpgrade(' + f.id + ')">升级</button>';
        }
        if (f.can_service) {
          html += ' <button class="btn btn-o btn-sm" title="启停 SAgent：整个 Agent 或指定插件" onclick="obService(' + f.id + ')">启停</button>';
        }
        html += '</td>';
        html += '</tr>';
      }
      html += '</tbody></table>';
    }
    html += '</div></div>';
    el.innerHTML = html;
  }).catch(function (e) {
    el.innerHTML = '<div class="card"><div class="card-bd"><div class="ob-empty">接入中心加载失败：' + obEscape(e && e.message || e) + '</div></div></div>';
  });
}

// ===================================================================
//  变更中心（前端效果设计 3.5 阶段3）：聚合「接入/升级/卸载/启停流水线 + 审计操作」
//  为统一变更时间线。每单含编码 / 类型 / 目标 / 状态 / 时间 / 步骤证据。
//  真实记录聚合，无伪审批入口（原生型人工批准由流水线人工步骤承担，符合原型"不伪造"精神）。
// ===================================================================
window.renderChanges = function () {
  var el = document.getElementById('main-content');
  el.innerHTML = '<div class="card"><div class="card-hd">📋 变更中心'
    + ' <span style="font-weight:400;font-size:11px;color:var(--muted)">接入流水线 + 审计 + 任务历史 · 统一变更时间线</span>'
    + '<button class="btn btn-p btn-sm" style="float:right" onclick="openOnboardNew()">➕ 新接入</button></div>'
    + '<div class="card-bd" id="ch-body"><div style="text-align:center;color:var(--muted);padding:20px">⏳ 汇总变更流水线与审计…</div></div></div>';
  Promise.all([
    fetch(API + '/onboard/flows').then(function (r) { return r.json(); }).catch(function () { return {}; }),
    fetch(API + '/audit?limit=1000').then(function (r) { return r.json(); }).catch(function () { return []; }),
    fetch(API + '/tasks?limit=1000').then(function (r) { return r.json(); }).catch(function () { return []; })
  ]).then(function (out) {
    var flows = (out[0] && out[0].flows) || [];
    var audit = out[1] || [];
    var tasks = out[2] || [];
    var entries = [];
    flows.forEach(function (f) {
      entries.push({
        ts: f.updated_at || f.created_at || '', code: 'flow-' + f.id,
        type: chFlowType(f), target: f.resource_id || f.resource_ip || f.agent_id || '—',
        status: chFlowStatus(f), evidence: f.current_title || f.current_step || '—',
        steps: (f.step_ids || []).length, flow: f
      });
    });
    audit.forEach(function (e) {
      var time = e.time || e.Time || '', action = e.action || e.Action || '',
        target = e.target || e.Target || '', result = e.result || e.Result || '',
        operator = e.operator || e.Operator || '';
      if (!time && !action) return;
      entries.push({
        ts: time, code: '审计', type: action, target: target,
        status: result.indexOf('失败') >= 0 ? 'failed' : 'done',
        evidence: operator ? '[' + operator + '] ' + result : result, steps: 0
      });
    });
    // 任务历史（/api/tasks）：人/运维操作留痕；与审计按 (action,time,target) 复合 key 去重，
    // 避免同一操作在「审计」与「任务」两来源下重复计入时间线
    var keySeen = {};
    entries.forEach(function (x) {
      var k = (x.type || '') + '|' + (x.ts || '') + '|' + (x.target || '');
      if (k !== '||') keySeen[k] = true;
    });
    tasks.forEach(function (t) {
      var time = t.time || t.Time || '', action = t.action || t.Action || '',
        target = t.target || t.Target || '', result = t.result || t.Result || '',
        operator = t.operator || t.Operator || '';
      if (!time && !action) return;
      var key = action + '|' + time + '|' + target;
      if (keySeen[key]) return; // 已在审计中呈现，跳过防重复
      keySeen[key] = true;
      entries.push({
        ts: time, code: 'task', type: action, target: target,
        status: result.indexOf('失败') >= 0 ? 'failed' : 'done',
        evidence: operator ? '[' + operator + '] ' + result : result, steps: 0
      });
    });
    entries.sort(function (a, b) { return String(b.ts).localeCompare(String(a.ts)); });

    var nRun = 0, nDone = 0, nFail = 0;
    entries.forEach(function (x) {
      if (x.status === 'running' || x.status === 'stalled') nRun++;
      else if (x.status === 'done') nDone++;
      else if (x.status === 'failed') nFail++;
    });

    var h = '<div class="ob-stat">';
    h += '<div class="s"><div class="n">' + entries.length + '</div><div class="l">变更总数</div></div>';
    h += '<div class="s"><div class="n" style="color:var(--warn)">' + nRun + '</div><div class="l">进行中</div></div>';
    h += '<div class="s"><div class="n" style="color:var(--success)">' + nDone + '</div><div class="l">成功</div></div>';
    h += '<div class="s"><div class="n" style="color:var(--error)">' + nFail + '</div><div class="l">失败</div></div>';
    h += '</div>';

    if (!entries.length) {
      h += '<div class="ob-empty">还没有任何变更记录。接入流水线、升级/卸载/启停与审计操作会在这里聚合为统一时间线。</div>';
    } else {
      // 列宽交给 table-layout:fixed（.ch-tab）——之前用 auto 布局时，前五列都是
      // 全局 td{white-space:nowrap}，长内容会把它们顶宽，反过来把唯一可换行的
      // 「证据 / 当前环节」挤到最窄；fixed 后宽度才是定死的，窄屏改为横向滚动
      h += '<div class="ch-scroll"><table class="ch-tab"><thead><tr>'
        + '<th style="width:150px">时间</th><th style="width:66px">编码</th><th style="width:124px">类型</th>'
        + '<th style="width:168px">目标</th><th style="width:88px">状态</th><th>证据 / 当前环节</th></tr></thead><tbody>';
      entries.forEach(function (x) {
        var badge = chBadge(x.status);
        // 证据正文一律走 obPlainTech 提纯：审计/任务行里那些
        // 「自动回报：ansible XXX完成 目标=… 家目录=…」的前缀在列表里毫无信息量，
        // 却占了整列宽度；核对数值这类真正有用的尾巴保留
        var evTxt = obPlainTech(x.evidence);
        var evEl = x.flow
          ? (x.steps ? '<span class="ch-steps">' + x.steps + ' 步</span>' : '') + '<button class="btn btn-o btn-sm" style="margin-left:8px" onclick="openOnboardFlow(' + x.flow.id + ')">查看过程</button>'
          : '';
        h += '<tr><td style="font-size:12px">' + obEscape(x.ts) + '</td>'
          + '<td><code class="ch-code">' + obEscape(x.code) + '</code></td>'
          + '<td title="' + obEscape(x.type) + '"><span class="ch-type">' + obEscape(x.type) + '</span></td>'
          + '<td style="font-size:12px" title="' + obEscape(x.target) + '">' + obEscape(x.target) + '</td>'
          + '<td>' + badge + '</td>'
          + '<td class="ch-ev">' + obEscape(evTxt) + evEl + '</td></tr>';
      });
      h += '</tbody></table></div>';
    }
    var body = document.getElementById('ch-body');
    if (body) body.innerHTML = h;
  }).catch(function (e) {
    var body = document.getElementById('ch-body');
    if (body) body.innerHTML = '<div style="color:#991b1b;font-size:12px;padding:12px">变更中心加载失败：' + obEscape(e && e.message || e) + '</div>';
  });
};

function chFlowType(f) {
  if (f.is_offboard || f.mode === 'offboard') return '卸载';
  if (f.mode === 'upgrade') return '升级';
  if (f.mode === 'service') return '启停';
  if (f.mode === 'fix') return '修复';
  return '接入';
}
function chFlowStatus(f) {
  if (f.status === 'running' || f.status === 'stalled') return f.status;
  if (f.status === 'ok' || f.status === 'done') return 'done';
  if (f.status === 'failed') return 'failed';
  if (f.status === 'canceled' || f.status === 'cancelled') return 'unknown';
  return 'unknown';
}
function chBadge(s) {
  var map = { running: 'run', stalled: 'stall', done: 'done', failed: 'failed', unknown: 'unk' };
  var cls = map[s] || 'unk';
  var txt = { run: '◐ 进行中', stall: '⏸ 卡住', done: '✓ 成功', failed: '✕ 失败', unk: '· 未知' }[cls];
  return '<span class="ch-badge ' + cls + '">' + txt + '</span>';
}

var OB_ABILITIES = []; // 本次向导的能力清单（后端下发）

function openOnboardNew() {
  Promise.all([
    fetch(API + '/onboard/templates').then(function (r) { return r.json(); }).catch(function () { return {}; }),
    fetch(API + '/onboard/abilities').then(function (r) { return r.json(); }).catch(function () { return {}; }),
    fetch(API + '/resources').then(function (r) { return r.json(); }).catch(function () { return []; })
  ]).then(function (res) {
    var tpls = (res[0] && res[0].templates) || [];
    OB_ABILITIES = (res[1] && res[1].abilities) || [];
    var list = (res[2] && res[2].resources) || (Array.isArray(res[2]) ? res[2] : []);
    renderOverlay('➕ 新建接入', function () { return obNewForm(tpls, OB_ABILITIES, list); }, true);
  });
}

function obFindAbility(id) {
  for (var i = 0; i < OB_ABILITIES.length; i++) {
    if (OB_ABILITIES[i].id === id) return OB_ABILITIES[i];
  }
  return null;
}

function obNewForm(tpls, abilities, resources) {
  var h = '';
  h += '<div style="font-size:12px;color:var(--muted);margin-bottom:12px">资源对象暂无外部来源时，直接手填 IP 或资源 ID。支持逗号 / 换行批量输入，每行起一条流水线。</div>';

  // ① 资源
  h += '<div style="margin-bottom:14px"><b style="font-size:13px">① 资源对象</b>';
  if (resources.length) {
    h += '<div style="margin:6px 0;max-height:120px;overflow:auto;border:1px solid var(--border);border-radius:6px">';
    for (var i = 0; i < resources.length; i++) {
      var r = resources[i];
      h += '<div style="padding:5px 10px;font-size:12px;border-bottom:1px solid var(--border);display:flex;justify-content:space-between;align-items:center"><label style="cursor:pointer;flex:1"><input type="checkbox" class="ob-res-cb" value="' + obEscape(r.id) + '"> <b>' + obEscape(r.id) + '</b> <span style="color:var(--muted)">' + obEscape(r.ip || '') + ' · ' + obEscape(r.os || '未知OS') + '/' + obEscape(r.arch || '?') + '</span></label><a onclick="obDelRes(\'' + obEscape(r.id) + '\')" style="cursor:pointer;color:var(--error);font-size:11px;opacity:.75">删除</a></div>';
    }
    h += '</div>';
  }
  h += '<textarea id="ob-res-input" rows="3" style="width:100%;padding:8px;border:1px solid var(--border);border-radius:4px;font-family:ui-monospace,Menlo,monospace;font-size:12px" placeholder="IP 或资源 ID（逗号 / 换行分隔）&#10;例：10.20.30.40&#10;例：mysql-prod-01, 10.20.30.41"></textarea>';
  h += '<div style="font-size:11px;color:var(--muted);margin-top:4px">资源台账里没有 OS / 架构时先留空——主机探路会实测后回填，实测值权威。</div></div>';

  // ①b SSH 凭据（随资源录入；现阶段人工填写模拟资源台账 get，接 CMDB 后由资源服务下发）
  h += '<div style="margin-bottom:14px"><b style="font-size:13px">① SSH 连接凭据</b>';
  h += '<div style="font-size:11px;color:var(--muted);margin:4px 0 6px">平台持有凭据后，<b>主机探路 / 安装 SAgent</b> 由平台 ansible 自动执行（联通性测试、环境探测、二进制分发、服务启动全自动）；不填则这些环节回落「人工复制执行包 + 回报结果」模式。密码仅用于执行，界面不回显明文。</div>';
  h += '<div style="display:flex;flex-wrap:wrap;gap:8px;align-items:center;font-size:12px">';
  h += '<label>SSH 端口 <input id="ob-ssh-port" size="6" placeholder="默认 22"></label>';
  h += '<label>账号 <input id="ob-ssh-user" size="12" placeholder="deploy / root"></label>';
  h += '<label>密码 <input id="ob-ssh-pass" size="14" type="password" placeholder="目标机 SSH 密码"></label>';
  h += '</div>';
  h += '<div style="font-size:11px;color:#92400e;background:#fef3c7;border:1px solid #fcd34d;border-radius:5px;padding:5px 8px;margin-top:6px">⚠ 端口留空时平台按默认 22 尝试，并在主机探路时读目标机 <code>sshd_config</code> 的 Port 声明与本机监听端口做核对：对不上会在步骤上直接告警。<b>现有主机若改过 SSH 端口（如 22022），请如实填写。</b></div></div>';

  // ①c 目标类型（2026-09-22 用户拍板：只填 IP + 类型 + 凭据，接入点平台全自动——
  //     探路时平台从目标机实测候选地址/自动建反向隧道并回写台账，用户永远不用碰"接入点"概念）
  h += '<div style="margin-bottom:14px"><b style="font-size:13px">① 目标类型</b>';
  h += '<div style="display:flex;gap:10px;margin-top:6px" id="ob-kind-wrap">';
  h += '<label style="flex:1;border:1px solid var(--border);border-radius:6px;padding:8px 10px;cursor:pointer;font-size:12px">';
  h += '<input type="radio" name="ob-kind" value="host" checked> <b>真实主机</b>';
  h += '<div style="color:var(--muted);margin-top:3px">公司内网虚拟机/物理机。直连不通时平台自动建立回连隧道并保活。</div></label>';
  h += '<label style="flex:1;border:1px solid var(--border);border-radius:6px;padding:8px 10px;cursor:pointer;font-size:12px">';
  h += '<input type="radio" name="ob-kind" value="docker"> <b>Docker 容器</b>';
  h += '<div style="color:var(--muted);margin-top:3px">本机/同 Docker 网络的容器目标。平台自动探测可用回连地址。</div></label>';
  h += '</div></div>';

  // ② 模式（模板由后端下发）
  h += '<div style="margin-bottom:14px"><b style="font-size:13px">② 采集模式</b><div style="display:flex;gap:10px;margin-top:6px" id="ob-mode-wrap">';
  if (!tpls.length) {
    h += '<div style="font-size:12px;color:var(--error)">模式模板加载失败：后端未下发模板，无法接入。请检查 data/flow_templates 是否随镜像就位。</div>';
  }
  for (var j = 0; j < tpls.length; j++) {
    var t = tpls[j];
    h += '<label style="flex:1;border:1px solid var(--border);border-radius:6px;padding:8px 10px;cursor:pointer;font-size:12px">';
    h += '<input type="radio" name="ob-mode" value="' + obEscape(t.id) + '"' + (j === 0 ? ' checked' : '') + '> <b>' + obEscape(t.name || t.id) + '</b>';
    h += '<div style="color:var(--muted);margin-top:3px">' + obEscape(t.description || '') + '</div></label>';
  }
  h += '</div></div>';

  // ③ 能力（清单由后端下发，locked 为地基能力）
  h += '<div style="margin-bottom:14px"><b style="font-size:13px">③ 采集能力</b>';
  var anyLocked = false;
  for (var k = 0; k < abilities.length; k++) { if (abilities[k].locked) anyLocked = true; }
  h += '<div style="font-size:11px;color:var(--muted);margin:4px 0 6px">' +
    (anyLocked ? '带 🔒 的是地基能力，承载平台能力的机器上强制开启、不可关闭。' : '勾选本次要接入的采集能力。') + '</div>';
  if (!abilities.length) {
    h += '<div style="font-size:12px;color:var(--error)">能力清单加载失败：后端未下发 onboard_plugins / agent_abilities。</div>';
  }
  h += '<div id="ob-abil-wrap" style="display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:8px;font-size:12px">';
  for (var m = 0; m < abilities.length; m++) {
    var a = abilities[m];
    h += '<label style="border:1px solid var(--border);border-radius:6px;padding:7px 10px;cursor:' + (a.locked ? 'not-allowed' : 'pointer') + ';' + (a.locked ? 'background:var(--bg);' : '') + '">';
    h += '<input type="checkbox" class="ob-abil-cb" value="' + obEscape(a.id) + '"' + (a.locked ? ' checked disabled' : '') + ' onchange="obSyncParams()"> <b>' + obEscape(a.name || a.id) + '</b>' + (a.locked ? ' 🔒' : '');
    h += '<div style="color:var(--muted);font-size:11px;margin-top:2px">' + obEscape(a.desc || a.id) + '</div></label>';
  }
  h += '</div></div>';

  // ④ 参数（字段声明来自插件包 params.yaml；没选带参数的能力时不占版面）
  h += '<div id="ob-params-wrap" style="margin-bottom:14px"><b style="font-size:13px">④ 采集参数</b><div id="ob-params" style="margin-top:6px"></div></div>';

  h += '<div style="display:flex;gap:8px"><button class="btn btn-p" onclick="submitOnboardNew()">🚀 起一条接入流水线</button>';
  h += '<button class="btn btn-o" onclick="closeOverlay()">取消</button></div>';

  setTimeout(obSyncParams, 0);
  return h;
}

// obSyncParams 按当前勾选重建参数表单（字段全部来自后端下发的 params 声明）
function obSyncParams() {
  var wrap = document.getElementById('ob-params');
  if (!wrap) return;
  var h = '';
  document.querySelectorAll('.ob-abil-cb').forEach(function (c) {
    if (!c.checked) return;
    var a = obFindAbility(c.value);
    if (!a || !a.params || !a.params.length) return;
    h += '<div style="border:1px solid var(--border);border-radius:6px;padding:8px 10px;margin-bottom:8px">';
    h += '<div style="font-size:12px;font-weight:600;margin-bottom:6px">' + obEscape(a.name || a.id) +
      ' <span style="color:var(--muted);font-weight:400">' + obEscape(a.id) + '</span></div>';
    for (var i = 0; i < a.params.length; i++) {
      var f = a.params[i];
      var attrs = ' class="ob-param" data-ability="' + obEscape(a.id) + '" data-field="' + obEscape(f.name) + '"' +
        (f.required ? ' data-required="1"' : '') +
        ' style="flex:1;padding:6px 8px;border:1px solid var(--border);border-radius:4px;font-size:12px"';
      h += '<div style="display:flex;gap:8px;align-items:flex-start;margin:5px 0">';
      h += '<label style="width:130px;font-size:12px;color:var(--muted);padding-top:6px">' +
        obEscape(f.label || f.name) + (f.required ? ' <span style="color:var(--error)">*</span>' : '') + '</label>';
      h += '<div style="flex:1">';
      if (f.type === 'textarea') {
        h += '<textarea rows="3"' + attrs + '>' + obEscape(f.default || '') + '</textarea>';
      } else if (f.type === 'enum' && f.options && f.options.length) {
        h += '<select' + attrs + '>';
        for (var o = 0; o < f.options.length; o++) {
          h += '<option value="' + obEscape(f.options[o]) + '"' + (f.options[o] === f.default ? ' selected' : '') + '>' + obEscape(f.options[o]) + '</option>';
        }
        h += '</select>';
      } else {
        h += '<input type="' + (f.type === 'secret' ? 'password' : 'text') + '" value="' + obEscape(f.default || '') +
          '" placeholder="' + obEscape(f.placeholder || '') + '"' + attrs + '>';
      }
      if (f.help) h += '<div style="font-size:11px;color:var(--muted);margin-top:2px">' + obEscape(f.help) + '</div>';
      h += '</div></div>';
    }
    h += '</div>';
  });
  wrap.innerHTML = h || '<div style="font-size:12px;color:var(--muted)">所选能力无需填写参数。</div>';
}

// obCollectParams 收集参数表单的值（字段名全部来自后端下发的 params 声明）+ 必填校验
function obCollectParams() {
  var out = {};
  var missing = null;
  document.querySelectorAll('.ob-param').forEach(function (el) {
    var ab = el.getAttribute('data-ability'), fd = el.getAttribute('data-field');
    if (!out[ab]) out[ab] = {};
    var v = el.value || '';
    out[ab][fd] = v;
    if (!missing && el.getAttribute('data-required') === '1' && !String(v).trim()) {
      var a = obFindAbility(ab);
      missing = (a ? (a.name || a.id) : ab) + ' → ' + fd;
    }
  });
  return { params: out, missing: missing };
}

// obDelRes 删除资源对象（含其 SSH 凭据）。活跃流水线守卫由后端把关：
// 有 running/blocked 流水线引用时后端拒绝，这里只如实转述拒绝原因
function obDelRes(id) {
  if (!confirm('删除资源对象 ' + id + '？\n\n将同时删除其 SSH 凭据与目标类型登记；\n名下未终态的接入流水线会被后端拒绝（先取消/走完再删）。\n\n此操作留痕审计。')) return;
  fetch(API + '/resources?id=' + encodeURIComponent(id), { method: 'DELETE' })
    .then(function (r) { return r.json(); })
    .then(function (d) {
      if (d && d.error) { toast('删除失败：' + d.error, 'err'); return; }
      toast('已删除 ' + id, 'ok');
      if (currentPage === 'onboard-center') renderOnboardCenter();
    })
    .catch(function (e) { toast('删除失败：' + (e && e.message || e), 'err'); });
}

function submitOnboardNew() {
  var picked = [];
  document.querySelectorAll('.ob-res-cb:checked').forEach(function (c) { picked.push(c.value); });
  var typed = (document.getElementById('ob-res-input').value || '').split(/[\n,，]/);
  for (var i = 0; i < typed.length; i++) {
    var v = typed[i].trim();
    if (v) picked.push(v);
  }
  if (!picked.length) { toast('请填写或选择一个资源对象', 'err'); return; }

  var mode = '';
  var mr = document.querySelector('input[name="ob-mode"]:checked');
  if (mr) mode = mr.value;
  if (!mode) { toast('模式模板未加载，无法创建', 'err'); return; }

  var abilities = [];
  document.querySelectorAll('.ob-abil-cb').forEach(function (c) { if (c.checked) abilities.push(c.value); });

  var collected = obCollectParams();
  if (collected.missing) { toast('必填参数未填：' + collected.missing, 'err'); return; }

  // 目标类型（接入点平台全自动，不再人工填写 agent_console_url）
  var kind = '';
  var kr = document.querySelector('input[name="ob-kind"]:checked');
  if (kr) kind = kr.value;

  // —— 预演回调：提交前先展示「接入影响摘要」，用户核对后确认才真正起流水线 ——
  // 对齐前端效果设计第3章「三步向导 → 预演影响 → 确认提交」：不在摘要阶段就发指令。
  var abilNames = [];
  abilities.forEach(function (id) { var a = obFindAbility(id); if (a) abilNames.push(a.name || id); });
  var modeName = mode;
  for (var tci = 0; tci < (window.__obTpls || []).length; tci++) { if (window.__obTpls[tci].id === mode) modeName = window.__obTpls[tci].name || mode; }
  renderOverlay('📋 接入影响预演', function () {
    var h = '<div style="font-size:12.5px;line-height:1.8">';
    h += '将在 <b>' + picked.length + ' 个资源对象</b> 上发起一条接入流水线，请核对本次影响范围：</div>';
    h += '<table style="width:100%;font-size:12.5px;margin-top:10px;border-collapse:collapse"><tbody>';
    h += '<tr><td style="padding:6px 8px;color:var(--muted);width:110px">资源对象</td><td style="padding:6px 8px"><code>' + obEscape(picked.join(', ')) + '</code></td></tr>';
    h += '<tr><td style="padding:6px 8px;color:var(--muted)">目标类型</td><td style="padding:6px 8px">' + obEscape(kind === 'docker' ? 'Docker 容器' : '真实主机') + '</td></tr>';
    h += '<tr><td style="padding:6px 8px;color:var(--muted)">采集模式</td><td style="padding:6px 8px">' + obEscape(modeName) + '</td></tr>';
    h += '<tr><td style="padding:6px 8px;color:var(--muted)">采集能力</td><td style="padding:6px 8px">' + (abilNames.length ? abilNames.join(' · ') : '<span style="color:var(--muted)">（地基能力）</span>') + '</td></tr>';
    var sshp = parseInt(obVal('ob-ssh-port'), 10) || 22;
    h += '<tr><td style="padding:6px 8px;color:var(--muted)">SSH 连接</td><td style="padding:6px 8px">' + obEscape(obVal('ob-ssh-user') || '(未填)') + ' @ 端口 ' + sshp + (obVal('ob-ssh-pass') ? '（已填密码）' : '（未填密码 → 人工执行回馈模式）') + '</td></tr>';
    h += '</tbody></table>';
    h += '<div style="display:flex;gap:8px;margin-top:16px"><button class="btn btn-p" onclick="doSubmitOnboardNew()">✅ 确认创建</button><button class="btn btn-o" onclick="closeOverlay()">返回修改</button></div>';
    return h;
  }, true);
  // 记住本次选择，供 doSubmitOnboardNew 二次采集
  window.__obPending = { picked: picked, mode: mode, abilities: abilities, params: collected.params, kind: kind };
}

function doSubmitOnboardNew() {
  var p = window.__obPending;
  if (typeof p !== 'object' || !p) { closeOverlay(); return; }
  fetch(API + '/onboard/flow', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      resources: p.picked, mode: p.mode, abilities: p.abilities, params: p.params,
      ssh_port: parseInt(obVal('ob-ssh-port'), 10) || 0,
      ssh_user: obVal('ob-ssh-user'),
      ssh_password: obVal('ob-ssh-pass'),
      target_kind: p.kind
    })
  }).then(function (r) { return r.json(); }).then(function (d) {
    if (d && d.error) { toast('创建失败：' + d.error, 'err'); return; }
    // 向导凭据与资源台账冲突必须立刻说：平台按台账执行，不告知等于"我填了却没用"
    if (d && d.cred_conflicts && d.cred_conflicts.length) {
      var lines = [];
      for (var ci = 0; ci < d.cred_conflicts.length; ci++) lines.push(d.cred_conflicts[ci].resource_id + '：' + d.cred_conflicts[ci].note);
      alert('⚠ 向导填写的凭据与资源台账不一致（平台按台账执行）：\n\n' + lines.join('\n'));
    }
    toast('已创建 ' + ((d && d.created) || 0) + ' 条接入流水线', 'ok');
    closeOverlay();
    if (currentPage === 'onboard-center') renderOnboardCenter(); else goPage('onboard-center');
  }).catch(function (e) { toast('创建失败：' + (e && e.message || e), 'err'); });
}

// ===================================================================
//  单条流水线：纵向时间线（每步可展开原始数据）
// ===================================================================
function openOnboardFlow(id) {
  fetch(API + '/onboard/flow?id=' + id).then(function (r) { return r.json(); }).then(function (d) {
    var f = (d && d.flow) || {};
    // 详情页可能被列表/资源页/总览直接拉起（没走过「新建接入」表单），
    // 这时能力清单缓存是空的——不补拉的话「本次要采什么」只能显示能力 ID，运维看不懂。
    // 只对接入流水线补，卸载等运维流水线没有能力清单这回事
    var needAb = f.is_onboard && !OB_ABILITIES.length;
    var pre = needAb
      ? fetch(API + '/onboard/abilities').then(function (r2) { return r2.json(); }).catch(function () { return null; })
      : Promise.resolve(null);
    return pre.then(function (ab) {
      if (ab && ab.abilities) OB_ABILITIES = ab.abilities;
      renderOverlay(obFlowTitle(f, id), function () { return obFlowBody(d); }, true, renderOnboardCenter);
      // 打上流水线印记并开始自动刷新：轮询只认这个印记，
      // 弹层被关掉或人换到别的页面（弹层还在、印记却不是它）都会自行收工
      var body = document.getElementById('overlay-body');
      if (body) body.setAttribute('data-ob-flow', String(id));
      obFlowPollStart(id, d);
    });
  });
}

// obFlowTitle 弹层标题按流水线类别给（接入/卸载/升级/启停是四件事，标题别都叫「接入过程」）
function obFlowTitle(f, id) {
  var who = f.resource_id || id;
  if (f.is_offboard) return '🧭 卸载过程 · ' + who;
  if (f.is_onboard) return '🧭 接入过程 · ' + who;
  if (f.mode === 'upgrade') return '🧭 升级过程 · ' + who;
  if (f.mode === 'service') return '🧭 启停过程 · ' + who;
  return '🧭 流水线过程 · ' + who;
}

// obFlowKindOf 流水线类别 → {key,label,icon}。**单一事实源**：资源抽屉的「过程记录」列表
// 从这里取类别，避免多处各写一份迟早漂移
function obFlowKindOf(f) {
  if (f) {
    if (f.is_offboard) return { key: 'offboard', label: '卸载', icon: '🗑' };
    if (f.is_onboard) return { key: 'onboard', label: '接入', icon: '🧭' };
    if (f.mode === 'upgrade') return { key: 'upgrade', label: '升级', icon: '⬆' };
    if (f.mode === 'service') return { key: 'service', label: '启停', icon: '⏻' };
  }
  return { key: 'other', label: '', icon: '•' };
}

function obFlowBody(d) {
  if (!d || !d.flow) return '<div class="ob-empty">未找到该流水线</div>';
  var f = d.flow, evs = d.events || [];
  var S = obFlowSections(d);
  var h = obFlowSec('sum', S.sum);
  if (f.is_offboard || f.is_onboard || f.is_service) {
    // 步骤式操作台（卸载 / 接入 / 启停共用）：分区各自带稳定 id，自动刷新按 id 定位、
    // 只换内容变了的（见 obFlowPatch）。gate（人工决策，含按钮）与 dur（耗时表）
    // 同处一个网格但各自成区——耗时表每轮都在跳，若与闸门同区，人正在点按钮时
    // 卡片会被换掉，点击就落空了
    h += '<div class="ob-wz">'
      + obFlowSec('obj', S.obj)      // ① 目标对象 + 本次范围（已装插件 / 要采什么 / 对谁做什么）
      + obFlowSec('steps', S.steps)  // ② 核心环节引导条
      + obFlowSec('concl', S.concl)  // ③ 结论横幅
      + '<div class="ob-wz-grid">' + obFlowSec('gate', S.gate) + obFlowSec('dur', S.dur) + '</div>'
      + obFlowSec('term', S.term)    // ⑥ 实时执行日志
      + obFlowSec('check', S.check)  // ⑦ 最终校验
      + obFlowSec('det', S.det)      // ⑧ 全量明细（折叠）
      + '</div>';
  } else {
    h += obFlowStagesBody(f, evs);
  }
  return h + obFlowSec('act', S.act);
}

// obFlowSec 分区外壳：自动刷新按 id 找块、再按内容有没有变决定动不动 DOM。
// 「不刷界面」靠的就是这一步——没变的区块一个字节都不重写，折叠状态与滚动位置自然保住
function obFlowSec(name, html) {
  return '<div class="ob-flow-sec" id="ob-flow-sec-' + name + '">' + html + '</div>';
}

// obFlowSections 流水线详情的分区清单（名字 → HTML），键名即分区 id。
// 初次渲染与自动刷新共用这一份拼装：两处各写一遍迟早漂移，
// 表现就是「自动刷新后跟手动刷新看到的不一样」
function obFlowSections(d) {
  var f = d.flow, evs = d.events || [];
  var byStep = obWzByStep(evs);
  var S = { sum: obFlowSummary(f), act: obFlowActions(f) };
  if (f.is_offboard) {
    S.obj = obWzObject(f, byStep);       // ① 目标对象 + 已装采集插件清单
    S.steps = obWzStepper(f, byStep);    // ② 核心环节引导条
    S.concl = obWzConclusion(f, byStep); // ③ 结论横幅
    S.gate = obWzGate(f, byStep);        // ④ 二次确认闸门（人工决策入口）
    S.dur = obWzDuration(f, byStep);     // ⑤ 每步耗时表
    S.term = obWzTerminal(f, byStep);    // ⑥ 实时执行日志
    S.check = obWzFinalCheck(f, byStep); // ⑦ 最终卸载校验
    S.det = obWzDetails(f, evs, byStep); // ⑧ 全量明细（折叠）
  } else if (f.is_onboard) {
    // 接入向导：与卸载共用同一副八区骨架，差异只在内容（见 obOnb* 段注释）
    S.obj = obOnbObject(f, evs, byStep);       // ① 目标对象 + 本次要采什么
    S.steps = obOnbStepper(f, evs);            // ② 核心环节引导条（四段推进）
    S.concl = obOnbConclusion(f, evs, byStep); // ③ 结论横幅
    S.gate = obOnbGate(f, evs, byStep);        // ④ 人工决策闸门（选版 / 探路回报）
    S.dur = obOnbDuration(f, evs);             // ⑤ 每步耗时与进度
    S.term = obOnbTerminal(f, evs);            // ⑥ 实时执行日志
    S.check = obOnbFinalCheck(f, byStep);      // ⑦ 最终入库校验
    S.det = obOnbDetails(f, evs);              // ⑧ 全量明细（折叠）
  } else if (f.is_service) {
    // 启停向导：与卸载/接入共用同一副八区骨架，差异只在内容（见 obSvc* 段注释）
    S.obj = obSvcObject(f, evs, byStep);       // ① 目标对象 + 本次操作 + 目标机实测现状
    S.steps = obSvcStepper(f, evs, byStep);    // ② 核心环节引导条（含启停校验收尾节点）
    S.concl = obSvcConclusion(f, evs, byStep); // ③ 结论横幅
    S.gate = obSvcImpact(f, byStep);           // ④ 影响范围核对（启停无人工闸门）
    S.dur = obSvcDuration(f, evs);             // ⑤ 每步耗时与进度
    S.term = obSvcTerminal(f, evs);            // ⑥ 实时执行日志
    S.check = obSvcFinalCheck(f, byStep);      // ⑦ 启停校验
    S.det = obSvcDetails(f, evs);              // ⑧ 全量明细（折叠）
  }
  return S;
}

// obFlowSummary 顶部摘要 + 停滞/停等提示（状态与耗时随流程推进，自动刷新要跟着变）
function obFlowSummary(f) {
  var h = '<div style="display:flex;gap:16px;flex-wrap:wrap;font-size:12px;margin-bottom:4px">';
  h += '<div><span style="color:var(--muted)">资源</span> <b>' + obEscape(f.resource_id) + '</b> <span style="color:var(--muted)">' + obEscape(f.resource_ip || '') + '</span></div>';
  h += '<div><span style="color:var(--muted)">模式</span> <b>' + obEscape(f.mode_name || f.mode) + '</b></div>';
  var agentCell;
  if (f.is_offboard) {
    // 卸载流水线的 agent_id 是"预期承载机 ID"（= 资源 ID），不是接入关系——
    // 显示「未接入（预期 ID…）」会让人以为这台机器还没接入过，语义正好相反
    agentCell = '—（卸载不绑定 Agent）';
  } else if (f.agent_registered) {
    agentCell = obEscape(f.agent_id);
  } else if (f.agent_id) {
    agentCell = '<span style="color:var(--muted);font-weight:400">未接入（预期 ID：' + obEscape(f.agent_id) + '，安装完成后按此 ID 注册）</span>';
  } else {
    agentCell = '未指派';
  }
  h += '<div><span style="color:var(--muted)">Agent</span> <b>' + agentCell + '</b></div>';
  h += '<div>' + obBadge(f.status, f.wait_kind) + '</div>';
  if (f.warn_count) h += '<div><span class="ob-warn">⚠ ' + f.warn_count + ' 项待复核</span></div>';
  if (f.elapsed_sec != null) h += '<div><span style="color:var(--muted)">耗时</span> <b>' + obDur(f.elapsed_sec * 1000) + '</b></div>';
  h += '</div>';

  if (f.status === 'running' && f.stall_sec) {
    if (f.stalled) {
      h += '<div style="margin:8px 0;padding:8px 10px;border-radius:6px;background:#fef3c7;color:#92400e;font-size:12px">⚠️ 当前环节已等待 <b>' + f.stall_sec + 's</b>，超过停滞阈值。' + (f.stall_hint ? obEscape(f.stall_hint) : '') + '</div>';
    } else if (f.wait_kind === 'external' && !f.ssh_ready) {
      h += '<div style="margin:8px 0;padding:8px 10px;border-radius:6px;background:#e6f4f3;color:#0b6e66;font-size:12px">👤 等待<b>外部执行器</b>回报（合法耗时不设限，已等待 ' + f.stall_sec + 's）。在目标机完成操作后调探针脚本回填，或点该环节下的「回报结果」；执行器自动上报通道已接线（/api/onboard/flow/probe）。</div>';
    } else if (f.wait_kind === 'external') {
      h += '<div style="margin:8px 0;padding:8px 10px;border-radius:6px;background:#e6f4f3;color:#0b6e66;font-size:12px">🤖 平台 ansible 自动执行中（已运行 ' + f.stall_sec + 's）。联通性测试 / 环境探测 / 二进制分发由平台代执行，完成后自动回报，无需人工操作。</div>';
    } else {
      h += '<div style="margin:8px 0;padding:8px 10px;border-radius:6px;background:var(--bg);color:var(--muted);font-size:12px">当前环节执行中，已等待 ' + f.stall_sec + 's。</div>';
    }
  }
  // 停等人工 ≠ 被阻断：把语义说清楚，避免运维去排查一个不存在的故障
  if (f.status === 'blocked' && f.wait_kind === 'human') {
    h += '<div style="margin:8px 0;padding:8px 10px;border-radius:6px;background:#e6f4f3;color:#0b6e66;font-size:12px">👤 流程停在「' + obEscape(f.waiting_label || '待人工确认') + '」——这不是故障、也不是门禁拦截，需要人工在下方对应环节完成决策后才会继续。下游环节显示的「暂不执行」是正常等待。</div>';
  }
  return h;
}

// obFlowActions 底部动作区 + 自动刷新指示（让人知道界面自己在拉数据，不用手点刷新）
function obFlowActions(f) {
  var h = '<div style="margin-top:14px;display:flex;gap:8px;align-items:center;flex-wrap:wrap">';
  if (obFlowStatusLive(f.status)) {
    h += '<span class="ob-flow-live"><span class="dot"></span>自动刷新中 · 每 ' + (OB_FLOW_POLL_MS / 1000) + ' 秒拉一次</span>';
  }
  if (f.status === 'running') {
    h += '<button class="btn btn-o btn-sm" onclick="obRefreshFlow(' + f.id + ')">↻ 刷新</button>';
    var cancelLabel = f.is_offboard ? '取消卸载' : (f.is_service ? '取消启停' : (f.mode === 'upgrade' ? '取消升级' : '取消接入'));
    h += '<button class="btn btn-o btn-sm" onclick="obCancel(' + f.id + ')">' + cancelLabel + '</button>';
  }
  return h + '</div>';
}

// obFlowStagesBody 接入流水线的四环节块版面（接入语义：声明需求 / 探路定案 / 装机注册 / 验证入库）：
// 事件按所属环节归桶（桶内保持原顺序），每桶一个环节块——块头聚合进度，
// 块体内步骤卡片原样保留（停等/门禁/重试/原始输出粒度不动）。
// 不属于四环节的步骤（卸载/升级/启停等运维流程）归入「其他步骤」块，照旧完整渲染
function obFlowStagesBody(f, evs) {
  var h = '';
  var stMap = {};
  for (var k = 0; k < evs.length; k++) { if (evs[k].step) stMap[evs[k].step] = evs[k].status || 'pending'; }
  var byStage = {}, others = [];
  for (var i = 0; i < evs.length; i++) {
    var si = obStageIndex(evs[i].step);
    if (si < 0) { others.push(i); continue; }
    (byStage[si] = byStage[si] || []).push(i);
  }
  for (var s = 0; s < OB_STAGES.length; s++) {
    if (byStage[s]) h += obStageBlockStMap(OB_STAGES[s], byStage[s], evs, f, stMap);
  }
  if (others.length) {
    h += obStageBlockStMap({ no: '·', name: '其他步骤', executor: '', steps: [],
      pass: '不在四段内的步骤（卸载/升级/启停等运维流程）' }, others, evs, f, stMap);
  }
  // 四段看完，给一条唯一的出口（图 4）：人不需要自己拼"现在到底该谁动"
  h += obNextStepBar(f, evs);
  return h;
}

// ===================================================================
//  卸载向导操作台（2026-09-24 用户评审：从「详情卡」升级为「步骤式向导」）
//
//  进入卸载界面即一目了然：有几个核心环节 / 这台机器装了哪些采集插件 /
//  二次确认卸载范围 / 然后一步步自动跑（界面自动刷新，人只观测）/ 每步耗时可见 /
//  执行日志回流到屏上（报错给位置与内容，可当场重试）/ 最终有校验动作确认真的卸干净了。
//
//  数据全部来自真实事件（timelineOf 的 status/duration_ms/detail/summary），
//  不引入任何 fixtures；「卸载校验」是把 uninstall_agent 的三检（进程/目录/端口）
//  与 cleanup_platform 的注销结果在展示层合成一个收尾节点，不新增探测链。
// ===================================================================

// OB_WZ_STEPS 向导的核心环节（= 后端 offboard 模板的真实步骤，顺序一致）。
// 引导条上「有几个必须的环节」直接照此渲染，不硬编码数量
var OB_WZ_STEPS = [
  { id: 'pick_object',       nm: '选择对象' },
  { id: 'scan_collectors',   nm: '扫描范围' },
  { id: 'confirm_uninstall', nm: '确认范围' },
  { id: 'uninstall_plugins', nm: '卸载插件' },
  { id: 'cleanup_autostart', nm: '清理自愈' },
  { id: 'uninstall_agent',   nm: '卸载 SAgent' },
  { id: 'cleanup_platform',  nm: '注销登记' }
];

// obWzCls 步骤状态 → 引导条样式类
function obWzCls(st) {
  if (st === 'ok' || st === 'skipped') return 'ok';
  if (st === 'running') return 'run';
  if (st === 'fail') return 'fail';
  if (st === 'blocked') return 'wait';
  return '';
}

function obWzEvIdx(evs, step) {
  for (var i = evs.length - 1; i >= 0; i--) { if (evs[i].step === step) return i; }
  return 0;
}

// obWzClock 从时间戳里取 HH:MM:SS 供日志行前缀（取不到留空，不编造时间）
function obWzClock(s) {
  if (!s) return '';
  var m = String(s).match(/(\d{2}:\d{2}:\d{2})/);
  return m ? m[1] : '';
}

// obWzByStep 事件按步骤 id 建索引（同一步骤多条事件时取最后一条）。
// 向导各分区、初次渲染与自动刷新都从这里取数，口径唯一
function obWzByStep(evs) {
  var byStep = {};
  for (var i = 0; i < (evs || []).length; i++) {
    if (evs[i].step) byStep[evs[i].step] = evs[i];
  }
  return byStep;
}

// ① 目标对象摘要 + 这台机器上装了什么（来自 scan_collectors 的目标机实测）
function obWzObject(f, byStep) {
  var h = '<div class="ob-wz-obj">';
  h += '<div class="who"><span class="lbl">目标对象</span><b>' + obEscape(f.resource_id) + '</b>';
  h += '<span class="ip">' + obEscape(f.resource_ip || '') + (f.ssh_ready ? ' · 平台持有 SSH 凭据' : ' · 未登记 SSH 凭据') + '</span></div>';
  var scan = byStep['scan_collectors'];
  var d = scan ? obDetail(scan) : null;
  var sd = (d && d.scan) || d;
  var ps = (sd && sd.plugins) || [];
  var as = (sd && sd.autostart) || [];
  h += '<div class="pl"><span class="lbl">已安装采集插件（卸载范围清单）· ' + ps.length + ' 项' + (as.length ? ' · 自愈/自启 ' + as.length + ' 处' : '') + '</span>';
  if (ps.length) {
    h += '<div class="ob-wz-chips">';
    for (var i = 0; i < ps.length; i++) {
      var hot = (ps[i].process === 'yes' || ps[i].process === 'unknown');
      h += '<span class="ob-wz-chip' + (hot ? ' hot' : '') + '"><span class="g"></span>' + obEscape(ps[i].name) + '<span class="x">' + obEscape(obFormText(ps[i].form)) + '</span></span>';
    }
    h += '</div>';
  } else {
    h += '<div style="font-size:11px;color:var(--muted);margin-top:4px">' + (scan ? '目标机上未发现启用的采集插件' : '待第 2 步扫描后列出') + '</div>';
  }
  return h + '</div></div>';
}

// ② 核心环节引导条：一眼看清卸载 SAgent 有几个必须的环节、现在跑到哪、还剩几步
function obWzStepper(f, byStep) {
  var h = '<div class="ob-wz-steps">';
  for (var i = 0; i < OB_WZ_STEPS.length; i++) {
    var sp = OB_WZ_STEPS[i], e = byStep[sp.id];
    var st = e ? (e.status || 'pending') : 'pending';
    var cls = obWzCls(st);
    var mark = cls === 'ok' ? '✓' : (cls === 'fail' ? '✕' : (cls === 'wait' ? '!' : String(i + 1)));
    h += '<div class="ob-wz-st ' + cls + '"><div class="ring">' + mark + '</div><div class="nm">' + obEscape(sp.nm) + '</div></div>';
    h += '<div class="ob-wz-lk' + (cls === 'ok' ? ' on' : '') + '"></div>';
  }
  var vs = obWzVerifyState(byStep);
  h += '<div class="ob-wz-st ' + vs.cls + '"><div class="ring">' + vs.mark + '</div><div class="nm">卸载校验</div></div>';
  return h + '</div>';
}

// obWzVerifyState 校验收尾节点的状态：卸载 SAgent（三检）与注销登记都过才算校验通过
function obWzVerifyState(byStep) {
  var ua = (byStep['uninstall_agent'] || {}).status || 'pending';
  var cp = (byStep['cleanup_platform'] || {}).status || 'pending';
  if (ua === 'fail' || cp === 'fail') return { cls: 'fail', mark: '✕' };
  if (ua === 'ok' && cp === 'ok') return { cls: 'ok', mark: '✓' };
  if (ua === 'running') return { cls: 'run', mark: '✓' };
  return { cls: '', mark: '✓' };
}

// ③ 结论横幅：一句话回答「现在到哪了、要不要我操作、成功没」
function obWzConclusion(f, byStep) {
  var cls, icon, tt, sub;
  if (f.status === 'done') {
    cls = 'ok'; icon = '✓'; tt = '卸载成功 · 目标机已清理干净';
    sub = '全部环节已完成，卸载校验通过。资源台账已注销，之后可原样重新接入。';
  } else if (f.status === 'failed') {
    cls = 'fail'; icon = '✕'; tt = '卸载失败 · 需要你介入排查';
    sub = '流程在失败环节停止，未继续执行后续步骤。下方日志已标出报错位置与内容，可在原地重试该步。';
  } else if (f.status === 'cancelled') {
    cls = 'wait'; icon = '–'; tt = '卸载已取消 · 目标机未做任何改动';
    sub = '操作人员在确认环节取消了本次卸载，决策已留痕审计。';
  } else if (f.wait_kind === 'human') {
    cls = 'wait'; icon = '!'; tt = '等待你确认卸载范围 · 确认后自动执行';
    sub = '这不是故障：请在下方「卸载范围二次确认」核对清单后点确认，之后剩余环节会自动跑完，无需再手工干预。';
  } else if (f.status === 'running') {
    cls = 'run'; icon = '◐'; tt = '卸载进行中 · 不需要你操作';
    sub = '平台正在自动执行，界面每 3 秒自动刷新。每一步完成后自动进入下一环节，失败即停并给出原因。';
  } else {
    cls = 'wait'; icon = '⏸'; tt = '卸载待开始'; sub = '流水线尚未开始推进。';
  }
  var h = '<div class="ob-wz-concl ' + cls + '"><span class="dot">' + icon + '</span><div>';
  h += '<div class="tt">' + obEscape(tt) + '</div>';
  h += '<div class="sub">' + obEscape(sub) + '</div>';
  return h + '</div></div>';
}

// ④+⑤ 两栏：二次确认闸门 | 每步耗时
function obWzGrid(f, byStep) {
  return '<div class="ob-wz-grid">' + obWzGate(f, byStep) + obWzDuration(f, byStep) + '</div>';
}

// obWzGate 二次确认闸门：confirm_uninstall 停等时把实测范围摊开，人工点头才放行
function obWzGate(f, byStep) {
  var e = byStep['confirm_uninstall'];
  var st = e ? (e.status || 'pending') : 'pending';
  var d = e ? obDetail(e) : null;
  if (st === 'blocked' && d && d.waiting_for === 'human_confirm_offboard') {
    return '<div>' + obOffboardConfirm(f, d) + '</div>';
  }
  var right = st === 'ok' ? '已批准' : (st === 'fail' ? '未通过' : (st === 'running' ? '扫描中' : '待扫描出范围'));
  var h = '<div class="ob-wz-card"><div class="chd">🗂 卸载范围二次确认<span class="r">' + right + '</span></div>';
  if (st === 'ok') {
    h += '<div style="font-size:12px;color:var(--success)">✓ ' + obEscape(e.summary || '已确认卸载范围，流程继续执行') + '</div>';
  } else if (st === 'fail') {
    h += '<div style="font-size:12px;color:var(--error)">✕ ' + obEscape(e.summary || '确认环节未通过') + '</div>';
  } else {
    h += '<div style="font-size:12px;color:var(--muted)">扫描出目标机上的采集插件与自愈来源后，在此列出完整卸载范围清单，由你二次确认后才开始卸载——确认之前不会对目标机做任何改动。</div>';
  }
  return h + '</div>';
}

// obWzDuration 每步耗时表：逐环节耗时 + 总耗时（含等待的墙钟时间）
function obWzDuration(f, byStep) {
  var h = '<div class="ob-wz-card"><div class="chd">📈 每步耗时与进度<span class="r">自动记录</span></div><table class="ob-wz-dtab">';
  for (var i = 0; i < OB_WZ_STEPS.length; i++) {
    var sp = OB_WZ_STEPS[i], e = byStep[sp.id];
    var st = e ? (e.status || 'pending') : 'pending';
    var dur;
    if (e && e.duration_ms) dur = obDur(e.duration_ms);
    else if (st === 'running') dur = '进行中…';
    else dur = '—';
    h += '<tr class="' + obWzCls(st) + '"><td class="sn">' + (i + 1) + '</td><td>' + obEscape(e ? (e.title || sp.nm) : sp.nm) + '</td><td class="du">' + obEscape(dur) + '</td></tr>';
  }
  var tot = (f.elapsed_sec != null) ? obDur(f.elapsed_sec * 1000) : '—';
  h += '<tr class="tot"><td></td><td>总耗时' + (f.status === 'running' ? '（进行中）' : '') + '</td><td class="du">' + obEscape(tot) + '</td></tr>';
  return h + '</table></div>';
}

// ⑥ 实时执行日志终端
function obWzTerminal(f, byStep) { return obTermShell(f, obWzLogLines(f, byStep), obWzTermTitle(f)); }

// obTermShell 日志终端外壳：卸载 / 接入 / 启停三个向导共用同一副「终端」视觉
// （窗口条 + 逐行日志 + 失败就地重试）。抽成一份是为了改一处即三处生效——
// 三份拷贝各自漂移正是本次「启停界面与接入/卸载不一致」的成因之一
function obTermShell(f, lines, title) {
  var h = '<div class="ob-wz-term">';
  h += '<div class="bar"><span class="dots"><span class="d d1"></span><span class="d d2"></span><span class="d d3"></span></span>';
  h += '<span class="tt">' + obEscape(title) + '</span></div>';
  h += '<div class="bd">';
  if (!lines.length) h += '<div class="ob-wz-ln"><span class="tx">等待流程开始…</span></div>';
  for (var i = 0; i < lines.length; i++) {
    var l = lines[i];
    if (l.cursor) { h += '<div class="ob-wz-ln"><span class="tx"><span class="ob-wz-cur"></span></span></div>'; continue; }
    h += '<div class="ob-wz-ln ' + l.cls + '"><span class="ts">' + obEscape(l.ts) + '</span><span class="tx">' + l.tx + '</span></div>';
    if (l.action) {
      h += '<div class="ob-wz-ln fail" style="padding-left:22px;gap:6px">';
      h += '<button class="btn btn-o btn-sm" onclick="obRetry(' + f.id + ',\'' + obEscape(l.action) + '\')">↻ 重试该步</button>';
      h += '<button class="btn btn-d btn-sm" onclick="obForce(' + f.id + ',\'' + obEscape(l.action) + '\')">强制继续（需留痕）</button>';
      h += '</div>';
    }
  }
  return h + '</div></div>';
}

function obWzTermTitle(f) {
  var who = f.resource_ip ? ('deploy@' + f.resource_ip) : f.resource_id;
  if (f.status === 'done') return 'uninstall verify — ' + who + ' · ✓ 卸载校验通过';
  if (f.status === 'failed') return 'ansible-playbook uninstall.yml — ' + who + ' · ✕ 执行中断';
  return 'ansible-playbook uninstall.yml — ' + who;
}

// obWzPlain 把执行器原文翻成运维一眼能懂的大白话（2026-09-24 用户评审：
// 「自动回报：ansible 卸载完成 目标=ibomc@10.1.207.156:22022 家目录=/home/ibomc
//  核对=插件核对 plugin_process_left=0」这种门槛太高）。
// 门槛低 ≠ 信息少：原文一个字没删——核对数值以「技术细节」尾巴附在同一条日志上，
// 完整原文仍在下方「全部环节执行明细」的原始输出里，排障时照样能翻到。
function obWzPlain(step, e, dd, f) {
  var st = e.status || 'pending';
  var scan = dd && (dd.scan || (dd.detail && dd.detail.scan));
  if (step === 'pick_object') {
    return '已锁定目标机器 ' + ((f && (f.resource_ip || f.resource_id)) || '') + '，准备开始卸载';
  }
  if (step === 'scan_collectors') {
    if (st === 'running') return '正在检查这台机器上装了什么采集插件…';
    if (st === 'blocked') return '等待检查结果';
    if (st !== 'ok') return '检查失败：没能读出这台机器上的采集插件';
    var tot = scan ? (scan.plugins_total || 0) : 0;
    var pro = scan ? (scan.process_count || 0) : 0;
    var auto = scan ? (scan.autostart_count || 0) : 0;
    if (!tot && !auto) return '检查完毕：这台机器上没有额外的采集插件或自愈配置，卸载只影响 SAgent 本身';
    var s = '检查完毕：这台机器上有 ' + tot + ' 个采集插件';
    if (pro) s += '（其中 ' + pro + ' 个带独立进程）';
    s += '、' + auto + ' 处自愈/自启配置，卸载会一并清掉';
    return s;
  }
  if (step === 'confirm_uninstall') {
    if (st === 'blocked') return '等你核对上面的范围清单并确认';
    if (st !== 'ok') return '未确认，卸载不会继续';
    var op = (dd && dd.operator) || '操作人员';
    var pn = (dd && dd.approved_plugins) || 0, an = (dd && dd.approved_autostart) || 0;
    return op + ' 已确认卸载范围（' + pn + ' 个采集插件、' + an + ' 处自愈/自启），平台开始自动执行';
  }
  if (step === 'uninstall_plugins') {
    if (st === 'running') return '正在停止并清除采集插件…';
    if (st !== 'ok') return '插件没清干净：目标机上还有插件进程在运行';
    return '采集插件已全部停止并清除，目标机上不再有插件进程';
  }
  if (step === 'cleanup_autostart') {
    if (st === 'running') return '正在清理自愈守护与定时任务…';
    if (st !== 'ok') return '自愈清理没通过：守护进程或定时任务仍有残留';
    if (dd && dd.autostart_verdict === 'partial') {
      return '自愈守护已删除；但定时任务没能核实（目标机上读不到 crontab），需要你上机手工核对一下';
    }
    return '自愈守护与定时任务已清理干净，不会再自动把 SAgent 拉起来';
  }
  if (step === 'uninstall_agent') {
    if (st === 'running') return '正在卸载 SAgent 本体…';
    if (st !== 'ok') return 'SAgent 没卸干净：安装目录或进程仍然存在';
    return 'SAgent 本体已卸载：安装目录已删除、进程已退出、监听端口已关闭';
  }
  if (step === 'cleanup_platform') {
    if (st === 'running') return '正在注销平台登记…';
    if (st !== 'ok') return '平台登记注销失败';
    return '平台登记已注销：Agent 台账、抓取配置、采集目标都已清除；资源对象和 SSH 凭据保留，随时可原样重新接入';
  }
  return e.summary || '';
}

// obPlainTech 技术细节提纯：从执行器原文里剥掉「自动回报：」「目标=…」「家目录=…」这些
// 每次都一样、且界面上已有专门位置写明的前缀，只留真正有排障价值的核对数值。
// 变更中心的审计/任务行与卸载向导的日志终端共用它——两处都被同一类长串顶过宽度
function obPlainTech(s) {
  s = String(s || '');
  if (!s) return '';
  s = s.replace(/自动回报：/g, '').replace(/Agent 自动回报[^：]*：/g, '');
  s = s.replace(/目标=\S+/g, '').replace(/家目录=\S+/g, '').replace(/核对=/g, '');
  // 「ansible 卸载采集插件完成」这类执行器自报的名号，正文已经说了这一步在干嘛
  s = s.replace(/ansible\s+\S+完成/g, '');
  s = s.replace(/⚠\s*\d+\s*项待复核[^，。]*/g, '');
  return s.replace(/\s+/g, ' ').replace(/^[\s、，,]+|[\s、，,]+$/g, '').trim();
}

// OB_WZ_TECH_STEPS 只有这几步的原文里带「机器核对值」（dir_exists=no / plugin_process_left=0…），
// 值得附一条技术细节尾巴。其余步骤（选择对象 / 人工确认 / 注销登记）的原文只是正文的换一种说法，
// 再贴一遍就是噪声——那正是用户说的"太技术"
var OB_WZ_TECH_STEPS = {
  scan_collectors: 1, uninstall_plugins: 1, cleanup_autostart: 1, uninstall_agent: 1
};

// obWzTech 卸载向导日志的「技术细节」尾巴（见 obPlainTech）
function obWzTech(e) {
  return obPlainTech(e && e.summary);
}

// obWzWarnN 原文里的「⚠ N 项待复核」条数（拆出来单独着色，别让它埋在一长串里）
function obWzWarnN(e) {
  var m = String((e && e.summary) || '').match(/⚠\s*(\d+)\s*项待复核/);
  return m ? m[1] : '';
}

// obWzLogLines 把每步事件铺成终端日志行：大白话正文 + 技术细节尾巴；
// 失败步骤补「报错位置 + 报错内容 + 建议」，并就地给出重试入口
function obWzLogLines(f, byStep) {
  var lines = [];
  for (var i = 0; i < OB_WZ_STEPS.length; i++) {
    var sp = OB_WZ_STEPS[i], e = byStep[sp.id];
    if (!e) continue;
    var st = e.status || 'pending';
    var dd = obDetail(e);
    var ts = obWzClock(e.started_at || e.finished_at || e.created_at || '');
    var cls = st === 'ok' ? 'ok' : (st === 'running' ? 'run' : (st === 'fail' ? 'fail' : (st === 'blocked' ? 'warn' : '')));
    var mark = st === 'ok' ? '✓' : (st === 'running' ? '◐' : (st === 'fail' ? '✕' : (st === 'blocked' ? '⏸' : '·')));
    var tx = '<b>' + mark + '</b> ' + obEscape(e.title || sp.nm) + ' — ' + obEscape(obWzPlain(sp.id, e, dd, f));
    if (st === 'ok' && OB_WZ_TECH_STEPS[sp.id]) {
      var tech = obWzTech(e);
      if (tech) tx += ' <span class="det">（技术细节：' + obEscape(tech) + '）</span>';
    }
    var wn = obWzWarnN(e);
    if (wn) tx += ' <span class="ob-wz-warn">⚠ ' + obEscape(wn) + ' 项待复核（见下方明细）</span>';
    lines.push({ ts: ts, cls: cls, tx: tx });
    if (st === 'fail') {
      var bad = obLeadingDiag(dd);
      if (bad) lines.push({ ts: ts, cls: 'fail', tx: '&nbsp;&nbsp;↳ ' + (bad.code ? '[' + obEscape(bad.code) + '] ' : '') + obEscape(bad.message || '') });
      var pos = obWzFailPos(dd);
      if (pos) lines.push({ ts: ts, cls: 'fail', tx: '&nbsp;&nbsp;↳ 报错位置：<span class="pos">' + obEscape(pos) + '</span>' });
      if (bad && bad.hint) lines.push({ ts: ts, cls: 'warn', tx: '&nbsp;&nbsp;↳ 建议：' + obEscape(bad.hint) });
      lines.push({ ts: ts, cls: 'fail', action: e.step });
    }
  }
  if (f.status === 'running') lines.push({ cursor: true });
  return lines;
}

// obWzFailPos 从 ansible 任务清单里取「报错位置」——失败任务名 + 首行错误内容
function obWzFailPos(dd) {
  if (!dd || !dd.tasks) return '';
  for (var i = 0; i < dd.tasks.length; i++) {
    var t = dd.tasks[i] || {};
    if (t.status !== 'failed' && t.status !== 'unreachable') continue;
    var txt = String(t.stderr || t.msg || '').replace(/\s+/g, ' ').trim();
    if (txt.length > 120) txt = txt.slice(0, 120) + '…';
    return (t.name || '未命名任务') + (txt ? ' · ' + txt : '');
  }
  return '';
}

// ⑦ 最终卸载校验：进程 / 目录 / 端口三检（目标机自证）+ 平台注销，全过才判成功
function obWzFinalCheck(f, byStep) {
  var ua = byStep['uninstall_agent'];
  var uaSt = ua ? (ua.status || 'pending') : 'pending';
  var dd = ua ? obDetail(ua) : null;
  var vl = (dd && dd.verify_line) || '';
  var m = {};
  var re = /(dir_exists|process_exists|port_agent_listening)\s*=\s*([A-Za-z]+)/g, mm;
  while ((mm = re.exec(vl))) m[mm[1]] = mm[2];
  var cpSt = (byStep['cleanup_platform'] || {}).status || 'pending';
  var done = (uaSt === 'ok' && cpSt === 'ok');
  var failed = (uaSt === 'fail' || cpSt === 'fail');
  var checks = [
    { nm: '进程检测', ok: m.process_exists === 'no', raw: m.process_exists != null ? ('process_exists=' + m.process_exists) : '', desc: 'SAgent 进程已不存在' },
    { nm: '目录检测', ok: m.dir_exists === 'no', raw: m.dir_exists != null ? ('dir_exists=' + m.dir_exists) : '', desc: '安装目录已删除' },
    { nm: '端口检测', ok: m.port_agent_listening === 'no', raw: m.port_agent_listening != null ? ('port_agent_listening=' + m.port_agent_listening) : '', desc: '监听端口已关闭' },
    { nm: '平台注销', ok: cpSt === 'ok', raw: cpSt === 'ok' ? '台账已注销' : '', desc: '平台登记已注销' }
  ];
  var h = '<div class="ob-wz-check' + (done ? ' pass' : '') + '">';
  h += '<div class="hd">🧪 最终卸载校验（确认真的卸干净）<span class="r">';
  if (done) h += '<span style="color:var(--success)">✓ 全部通过</span>';
  else if (failed) h += '<span style="color:var(--error)">✕ 未通过</span>';
  else h += '<span style="color:var(--muted)">待执行</span>';
  h += '</span></div>';
  for (var i = 0; i < checks.length; i++) {
    var c = checks[i];
    var cls = c.ok ? 'ok' : (failed ? 'bad' : '');
    var mark = c.ok ? '✓' : (failed ? '✕' : '·');
    h += '<div class="ob-wz-ck ' + cls + '"><span class="st">' + mark + '</span><span class="nm">' + c.nm + '</span>';
    h += '<span style="color:var(--muted)">' + obEscape(c.desc) + '</span>';
    h += '<span class="vv">' + obEscape(c.raw || '待执行') + '</span></div>';
  }
  if (done) h += '<div style="margin-top:8px;font-size:12px;color:#065f46;font-weight:600">✔ 目标机已清理干净、平台台账已注销 —— SAgent 卸载成功，可随时原样重新接入。</div>';
  return h + '</div>';
}

// ⑧ 全量明细折叠屉：每步完整卡片（诊断/任务清单/尝试记录/原始输出/重试按钮）零删减
function obWzDetails(f, evs, byStep) {
  var h = '<div class="ob-wz-acc">';
  h += '<div class="h" onclick="obRowToggle(\'ob-wz-det\')"><span>🔍 全部环节执行明细与原始输出（排障用）</span><span class="ar" id="ob-wz-det-ar">▼ 明细</span></div>';
  h += '<div class="b" id="ob-wz-det" style="display:none">';
  for (var i = 0; i < OB_WZ_STEPS.length; i++) {
    var sp = OB_WZ_STEPS[i], e = byStep[sp.id];
    // confirm_uninstall 已在闸门区独占渲染，此处不再重复（信息不丢失）
    if (!e || sp.id === 'confirm_uninstall') continue;
    h += obStepCard(f, e, obWzEvIdx(evs, sp.id));
  }
  return h + '</div></div>';
}

// ===================================================================
//  接入向导操作台（2026-09-24 用户评审：只参考卸载界面的「版面与视觉」，
//  接入的流程定义按安装接入自己的语义来——声明需求 → 探路定案 → 装机注册 → 验证入库；
//  把接入详情从「环节块 + 步骤卡片」重做成同一套步骤式操作台）
//
//  与卸载向导共用同一副骨架（八个分区 + 自动刷新就地补丁），差异只在内容：
//    ① 目标对象 + 本次要采什么（采集能力）   ② 核心环节引导条（四段推进 + 段内进度）
//    ③ 结论横幅（到哪了 / 要不要我操作 / 成功没）  ④ 人工决策闸门（探路回报 / 版本选定）
//    ⑤ 每步耗时与进度                       ⑥ 实时执行日志终端（说人话 + 技术细节退括号）
//    ⑦ 最终入库校验（自监控 / 配置下发 / 连通验证 / 样本入库）  ⑧ 全量明细（排障用）
//
//  数据全部来自真实事件（timelineOf 的 status/duration_ms/detail/summary），
//  不引入任何 fixtures；「入库校验」是把四步既有结论在展示层合成一个收尾节点，
//  不新增探测链。
//
//  步骤清单不写死：接入有 edge/remote/hybrid 三种编排（10~13 步），
//  一律取 timelineOf 的下发结果（含未执行的 pending 步），新增模板不必改这里。
// ===================================================================

// obOnbStMap 步骤 → 状态（引导条聚合、耗时表、校验共用同一口径）
function obOnbStMap(evs) {
  var m = {};
  for (var i = 0; i < (evs || []).length; i++) { if (evs[i].step) m[evs[i].step] = evs[i].status || 'pending'; }
  return m;
}

// ① 目标对象 + 本次要采什么
function obOnbObject(f, evs, byStep) {
  var ab = f.abilities || [];
  var h = '<div class="ob-wz-obj">';
  h += '<div class="who"><span class="lbl">目标对象</span><b>' + obEscape(f.resource_id) + '</b>';
  h += '<span class="ip">' + obEscape(f.resource_ip || '') + (f.ssh_ready ? ' · 平台持有 SSH 凭据' : ' · 未登记 SSH 凭据') + '</span></div>';
  h += '<div class="who"><span class="lbl">采集模式</span><b>' + obEscape(f.mode_name || f.mode) + '</b>';
  h += '<span class="ip">' + obEscape(obOnbCarrier(f, byStep)) + '</span></div>';
  h += '<div class="pl"><span class="lbl">本次要采什么（勾选的采集能力）· ' + ab.length + ' 项</span>';
  if (ab.length) {
    h += '<div class="ob-wz-chips">';
    for (var i = 0; i < ab.length; i++) {
      var a = obFindAbility(ab[i]);
      h += '<span class="ob-wz-chip"><span class="g"></span>' + obEscape(a ? (a.name || ab[i]) : ab[i]) + '</span>';
    }
    h += '</div>';
  } else {
    h += '<div style="font-size:11px;color:var(--muted);margin-top:4px">' +
      (f.status === 'done' ? '本次接入未勾选额外采集能力' : '待「选择采集能力」环节完成后列出') + '</div>';
  }
  return h + '</div></div>';
}

// obOnbCarrier 承载机一行：远程/混合模式下 SAgent 装在代理机上（目标机零侵入）——
// 「到底哪台机器要装东西」是接入的核心信息，不能只藏在步骤明细里
function obOnbCarrier(f, byStep) {
  var e = byStep['pick_proxy'];
  if (e && e.status === 'ok' && e.summary) return e.summary;
  if (f.agent_registered) return 'SAgent 已注册：' + (f.agent_id || '');
  if (f.mode === 'remote' || f.mode === 'hybrid') return '承载机（代理机）待「确认承载主机」环节确定';
  return 'SAgent 装在本机（边缘采集）';
}

// ② 核心环节引导条：接入是一条四段推进（声明需求 → 探路定案 → 装机注册 → 验证入库，
// 2026-09-24 用户拍板：按安装接入自己的语义定义，不照搬卸载的步骤），
// 引导条按这四段出环，环下带段内进度 n/m——一眼看清跑到哪一段、这一段还剩几步。
// 段内步骤明细放 title 悬停，不把 13 个环挤在一行里
function obOnbStepper(f, evs) {
  var stMap = obOnbStMap(evs);
  var h = '<div class="ob-wz-steps">';
  for (var s = 0; s < OB_STAGES.length; s++) {
    var g = OB_STAGES[s], a = obStageAgg(g, stMap);
    var cls = a.cls;
    var mark = cls === 'ok' ? '✓' : (cls === 'fail' ? '✕' : g.no);
    // 悬停里把「这一段具体有哪几步」按本模板真实下发顺序铺出来——
    // 四段是展示层归组，不给步骤名的话人没法把环节和实际动作对上
    var names = [];
    for (var k = 0; k < evs.length; k++) {
      if (g.steps.indexOf(evs[k].step) >= 0) names.push(evs[k].title || evs[k].step);
    }
    var tip = g.no + ' ' + g.name + '（' + a.done + '/' + a.total + '）'
      + (names.length ? '\n本环节步骤：' + names.join(' → ') : '')
      + '\n通过条件：' + g.pass;
    h += '<div class="ob-wz-st ' + cls + '" title="' + obEscape(tip) + '"><div class="ring">' + obEscape(mark) + '</div>'
      + '<div class="nm">' + obEscape(g.name) + '</div><div class="sub">' + a.done + '/' + a.total + '</div></div>';
    if (s < OB_STAGES.length - 1) h += '<div class="ob-wz-lk' + (cls === 'ok' ? ' on' : '') + '"></div>';
  }
  return h + '</div>';
}

// obOnbWaitState 接入流程「现在卡在等谁」：把三类停等拆清楚——
//   pick_version  等人工点选版本（决策闸门就地给清单）
//   probe_report  等人工上机跑探路并把结果回报（无 SSH 凭据的兜底路径）
//   ansible       平台 ansible 正在自动执行（等就行，不需要人）
function obOnbWaitState(f, evs) {
  var out = { kind: '', step: '' };
  if (f.status === 'blocked' && f.waiting_for === 'human_pick') { out.kind = 'pick_version'; out.step = 'pick_version'; return out; }
  for (var i = 0; i < evs.length; i++) {
    var e = evs[i];
    if ((e.status || '') !== 'running' || e.scope !== 'external') continue;
    var dd = obDetail(e);
    if (dd && dd.executor === 'ansible') { out.kind = 'ansible'; out.step = e.step; return out; }
    if (dd && dd.instruction) { out.kind = 'probe_report'; out.step = e.step; return out; }
  }
  return out;
}

// ③ 结论横幅：一句话回答「现在到哪了、要不要我操作、成功没」
function obOnbConclusion(f, evs, byStep) {
  var ws = obOnbWaitState(f, evs);
  var cls, icon, tt, sub;
  if (f.status === 'done') {
    cls = 'ok'; icon = '✓'; tt = '接入成功 · 数据已在采集';
    sub = '全部环节已完成，入库校验通过。采集实况见「资源与指标」页这台主机的对应行。';
  } else if (f.status === 'failed') {
    cls = 'fail'; icon = '✕'; tt = '接入失败 · 需要你介入排查';
    sub = '流程在失败环节停止，未继续执行后续步骤。下方日志已标出报错位置与内容，可在原地重试该步。';
  } else if (f.status === 'cancelled' || f.status === 'canceled') {
    cls = 'wait'; icon = '–'; tt = '接入已取消';
    sub = '本次接入已取消，目标机上未做进一步改动。如需重新接入，在资源页对该主机点「接入」。';
  } else if (ws.kind === 'pick_version') {
    cls = 'wait'; icon = '!'; tt = '等你选定 SAgent 版本 · 选定后自动装机';
    sub = '这不是故障：主机探路已完成并给出兼容版本清单，请在下方「版本选定」中点选一个版本，之后剩余环节会自动跑完，无需再手工干预。';
  } else if (ws.kind === 'probe_report') {
    cls = 'wait'; icon = '!'; tt = '等你完成主机探路回报 · 回报后自动继续';
    sub = '这不是故障：平台未持有该主机的 SSH 凭据，请在下方「主机探路回报」按执行包在目标机跑一次并把结果填回来，平台据此选版并继续。';
  } else if (f.status === 'running') {
    cls = 'run'; icon = '◐'; tt = '接入进行中 · 不需要你操作';
    if (ws.kind === 'ansible') {
      sub = '平台 ansible 正在自动执行「' + (f.current_title || '当前环节') + '」，完成后自动回报并继续；界面每 4 秒自动刷新。';
    } else {
      sub = '平台正在自动推进，界面每 4 秒自动刷新。每一步完成后自动进入下一环节，失败即停并给出原因。';
    }
  } else {
    cls = 'wait'; icon = '⏸'; tt = '接入待开始'; sub = '流水线尚未开始推进。';
  }
  var h = '<div class="ob-wz-concl ' + cls + '"><span class="dot">' + icon + '</span><div>';
  h += '<div class="tt">' + obEscape(tt) + '</div>';
  h += '<div class="sub">' + obEscape(sub) + '</div>';
  return h + '</div></div>';
}

// ④ 人工决策闸门：接入的人工决策有三处（选承载采集机 / 选版本 / 回报探路），
// 就地渲染在这里——不再让人翻到步骤卡片里找入口（卸载向导把二次确认抬到闸门区同理）
function obOnbGate(f, evs, byStep) {
  // 远程/混合：先给「承载采集机」决策——选机（装在哪台机器上）与配被采对象（IP/账号/口令）
  // 是两步，本卡只做第一步，第二步在「采集参数」环节（2026-09-24 用户补充）
  var pre = obOnbCollectorCard(f, byStep);
  var ws = obOnbWaitState(f, evs);
  if (ws.kind === 'pick_version') {
    var ve = byStep['pick_version'], vd = ve ? obDetail(ve) : null;
    if (vd && vd.candidates && vd.candidates.length) return pre + '<div>' + obVersionPicker(f, vd) + '</div>';
    return pre + obOnbGateCard('🎯 版本选定', '待探路', '主机探路给出 OS/架构后，这里会列出兼容版本清单由你点选。');
  }
  if (ws.kind === 'probe_report') {
    var pe = byStep[ws.step], pd = pe ? obDetail(pe) : null;
    if (pd && pd.instruction) {
      var h = '<div class="ob-wz-card"><div class="chd">🧭 主机探路回报（人工执行）<span class="r">等你操作</span></div>';
      h += '<div style="font-size:12px;color:var(--muted);margin-bottom:4px">平台未持有该主机的 SSH 凭据，本环节由人工在目标机执行并把结果回报上来——回报后平台自动选版并继续。</div>';
      h += obExecutorBox(f, pd, 'onb-probe');
      return pre + h + '</div>';
    }
  }
  // 无人工决策：给一张状态卡，说明这一步谁在动、要不要人管
  var ve2 = byStep['pick_version'];
  var vst = ve2 ? (ve2.status || 'pending') : 'pending';
  if (vst === 'ok' || vst === 'skipped') {
    var vsum = obPlainTech(ve2 && ve2.summary) || (ve2 && ve2.summary) || '';
    return pre + obOnbGateCard('🎯 版本选定', '已选定', vsum ? 'SAgent 版本已人工选定：' + vsum : 'SAgent 版本已人工选定，决策已留痕审计。');
  }
  if (f.status === 'done') return pre + obOnbGateCard('🎯 版本选定', '已完成', '接入已办结，无需人工决策。');
  return pre + obOnbGateCard('🎯 人工决策', '无需操作', '本次接入目前没有需要你决策的环节——探路与安装由平台自动执行，有决策点时会在这里出现。');
}

// obOnbCollectorCard 承载采集机决策卡（远程采集模式）。
// 「选哪台采集机承载」与「配被采对象（IP/账号/口令）」是两步——本卡只做第一步。
// 候选清单来自 pick_proxy 步骤 detail（平台按池聚合下发：池 / 承载数 / 健康态），前端零写死。
// 混合采集不在此列：它的承载机就是被采主机自身（同机既采本机又采远端），无可挑。
function obOnbCollectorCard(f, byStep) {
  if (f.mode !== 'remote') return '';
  var e = byStep['pick_proxy'];
  if (!e) return '';
  var d = obDetail(e) || {};
  var pools = d.pools || [];
  var cur = d.agent_id || f.agent_id || '';
  var h = '<div class="ob-wz-card"><div class="chd">🖥 承载采集机 · 这一步选「装在哪台机器上」';
  h += '<span class="r">' + obEscape(obCollectorSrcText(d.source)) + '</span></div>';
  h += '<div style="font-size:12px;color:var(--muted);margin-bottom:6px">远程采集的 SAgent 装在<b>采集机</b>上，'
    + '被采对象（IP / 账号 / 口令）在下一步「采集参数」里配。候选按池分组，标注当前承载数与心跳健康态；'
    + '池内健康采集机不足 2 台时，该池故障后没有承接方（采集目标无法自动漂移）。</div>';
  if (!pools.length) {
    h += '<div style="font-size:12px;color:#92400e;background:#fef3c7;padding:8px 10px;border-radius:6px">'
      + '⚠ 本租户还没有登记采集机（<code>type=proxy</code> 或 <code>labels.role=collector</code>）。'
      + '未登记时按资源自身承载——<b>无法享受采集机故障后的自动漂移</b>，建议先登记采集机再接入。</div></div>';
    return h;
  }
  var editable = f.status !== 'done' && f.status !== 'cancelled' && f.status !== 'canceled';
  for (var i = 0; i < pools.length; i++) {
    var p = pools[i], cs = p.candidates || [];
    h += '<div style="margin:6px 0 2px;font-size:11px;color:var(--muted)">池 <b>' + obEscape(p.pool || '未登记') + '</b>'
      + ' · 健康 ' + (p.healthy_count || 0) + ' 台'
      + (p.insufficient ? ' <span style="color:#b45309">⚠ 池内不足 2 台，故障后无承接方（不可漂移）</span>' : '')
      + '</div>';
    for (var j = 0; j < cs.length; j++) {
      var c = cs[j];
      var checked = (c.agent_id === cur) ? ' checked' : ((c.recommended && !cur) ? ' checked' : '');
      h += '<label style="display:flex;gap:8px;align-items:center;padding:4px 0;font-size:12px;cursor:pointer' + (c.healthy ? '' : ';opacity:.75') + '">';
      h += '<input type="radio" name="ob-collector" value="' + obEscape(c.agent_id) + '"' + (editable ? '' : ' disabled') + checked + '>';
      h += '<b>' + obEscape(c.agent_id) + '</b>';
      h += '<span style="color:var(--muted)">承载 ' + (c.load || 0) + ' 个远端采集目标</span>';
      h += '<span class="badge ' + (c.healthy ? 'b-r' : 'b-o') + '" style="font-weight:400">' + (c.healthy ? '心跳正常' : '心跳陈旧') + '</span>';
      if (c.recommended) h += '<span class="badge b-r" style="font-weight:400">推荐</span>';
      if (c.region) h += '<span style="color:var(--muted)">' + obEscape(c.region) + '</span>';
      h += '</label>';
    }
  }
  if (editable) {
    h += '<div style="margin-top:8px"><button class="btn btn-d btn-sm" onclick="obPickCollector(' + f.id + ')">改选承载采集机</button>'
      + ' <span style="font-size:11px;color:var(--muted)">改选后本流水线已建的采集目标会一并改绑到新采集机</span></div>';
  }
  return h + '</div>';
}

// obCollectorSrcText 承载机的选定来源（自动挑机 / 人工改选 / 未登记回落本机）
function obCollectorSrcText(src) {
  var m = { auto_pick: '已自动挑机', selected: '已选定', human_pick: '已人工改选', fallback_self: '未登记采集机 · 按本机承载' };
  return m[src] || (src ? src : '待选');
}

// obPickCollector 人工改选承载采集机：改绑已建采集目标 + 留痕审计，随后续推流水线
function obPickCollector(fid) {
  var el = document.querySelector('input[name="ob-collector"]:checked');
  if (!el) { alert('请先选择一台承载采集机'); return; }
  if (!confirm('确认把承载采集机改为 ' + el.value + '？\n\n该流水线已建的采集目标会一并改绑到这台机器上，采集任务随即由它承担。操作将留痕审计。')) { return; }
  fetch('/api/onboard/flow/collector', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ flow_id: fid, agent_id: el.value })
  }).then(function (r) { return r.json(); }).then(function (d) {
    if (d.ok) {
      if (d.warning) alert('已改选，但请注意：' + d.warning);
      obRefreshFlow(fid);
    } else { alert('改选被拒：' + (d.error || '未知原因')); }
  }).catch(function (e) { alert('请求失败：' + e); });
}

function obOnbGateCard(title, right, body) {
  return '<div class="ob-wz-card"><div class="chd">' + obEscape(title) + '<span class="r">' + obEscape(right) + '</span></div>'
    + '<div style="font-size:12px;color:var(--muted)">' + obEscape(body) + '</div></div>';
}

// ⑤ 每步耗时与进度：逐环节耗时 + 总耗时（含等待的墙钟时间）。
// 步骤清单取时间线（本模板真实编排），不写死
function obOnbDuration(f, evs) {
  var h = '<div class="ob-wz-card"><div class="chd">📈 每步耗时与进度<span class="r">自动记录</span></div><table class="ob-wz-dtab">';
  for (var i = 0; i < evs.length; i++) {
    var e = evs[i], st = e.status || 'pending';
    var dur;
    if (e.duration_ms) dur = obDur(e.duration_ms);
    else if (st === 'running') dur = '进行中…';
    else dur = '—';
    h += '<tr class="' + obWzCls(st) + '"><td class="sn">' + (i + 1) + '</td><td>' + obEscape(e.title || e.step)
      + (st === 'skipped' ? '<span style="color:var(--muted)">（不适用，已跳过）</span>' : '')
      + '</td><td class="du">' + obEscape(dur) + '</td></tr>';
  }
  var tot = (f.elapsed_sec != null) ? obDur(f.elapsed_sec * 1000) : '—';
  h += '<tr class="tot"><td></td><td>总耗时' + (f.status === 'running' ? '（进行中）' : '') + '</td><td class="du">' + obEscape(tot) + '</td></tr>';
  return h + '</table></div>';
}

// ⑥ 实时执行日志终端
function obOnbTerminal(f, evs) { return obTermShell(f, obOnbLogLines(f, evs), obOnbTermTitle(f)); }

function obOnbTermTitle(f) {
  var who = f.resource_ip ? ('deploy@' + f.resource_ip) : f.resource_id;
  if (f.status === 'done') return 'onboard verify — ' + who + ' · ✓ 数据已入库';
  if (f.status === 'failed') return 'ansible-playbook onboard.yml — ' + who + ' · ✕ 执行中断';
  return 'ansible-playbook onboard.yml — ' + who;
}

// OB_ONB_TECH_STEPS 只有这几步的原文里带「机器核对值」（sha256 / 实测档案 / 序列数…），
// 值得附一条技术细节尾巴。选择对象 / 选能力 / 选版本 / 注册登记这类步骤的原文
// 只是正文的换一种说法，再贴一遍就是噪声——那正是用户说的"太技术"
var OB_ONB_TECH_STEPS = {
  preflight_host: 1, preflight_remote: 1, install_agent: 1, verify_probe: 1, observe_collect: 1
};

// obOnbTech 接入向导日志的「技术细节」尾巴。
// Agent 回报类步骤的原文是「Agent 自动回报 kind：{json}」，直接提纯会剩一坨 JSON——
// 改为从结构化 data 里取真正有排障价值的核对值；其余步骤仍走 obPlainTech 提纯原文
function obOnbTech(e, dd) {
  var data = dd && dd.data;
  if (data && typeof data === 'object') {
    var parts = [];
    if (data.applied_version != null) parts.push('applied_version=' + data.applied_version);
    if (data.vm_series_count != null) parts.push('vm_series_count=' + data.vm_series_count);
    if (data.vm_verified != null) parts.push('vm_verified=' + data.vm_verified);
    if (data.series_count != null) parts.push('series_count=' + data.series_count);
    if (parts.length) return parts.join(' ');
  }
  return obPlainTech(e && e.summary);
}

// obOnbPlain 把执行器原文翻成运维一眼能懂的大白话（与卸载向导同一套门槛）。
// 门槛低 ≠ 信息少：原文一个字没删——核对数值以「技术细节」尾巴附在同一条日志上，
// 完整原文仍在下方「全部环节执行明细」里，排障时照样能翻到
function obOnbPlain(step, e, dd, f) {
  var st = e.status || 'pending';
  var ft = dd && dd.facts;
  var cnt = dd && dd.ability && dd.ability.counts;
  if (step === 'pick_object') {
    return '已锁定目标机器 ' + ((f && (f.resource_ip || f.resource_id)) || '') + '，开始接入';
  }
  if (step === 'pick_proxy') {
    if (st === 'running') return '正在确定承载采集能力的机器…';
    if (st !== 'ok') return '承载采集机未确定';
    var pc = dd || {};
    if (pc.source === 'fallback_self') return '本租户未登记采集机，暂按资源自身承载（无法享受故障漂移）';
    return '已确定承载采集机：' + (obPlainTech(e.summary) || e.summary || '')
      + (pc.pool ? '，池 ' + pc.pool : '');
  }
  if (step === 'pick_ability') {
    if (st === 'running') return '正在记录本次要采什么…';
    if (st !== 'ok') return '采集能力未记录';
    return '本次要采的能力已记录：' + (obPlainTech(e.summary) || e.summary || '');
  }
  if (step === 'collect_params') {
    if (st === 'skipped') return '本次所选能力无需额外参数，跳过参数填写';
    if (st === 'running') return '正在落采集参数…';
    if (st !== 'ok') return '采集参数未落库';
    return '采集参数已落库，并已建好对应采集目标';
  }
  if (step === 'preflight_host' || step === 'preflight_remote') {
    if (st === 'running') return '正在实测目标主机与所选能力的可行性…';
    if (st === 'blocked') return '等待主机探路实测结果';
    if (st !== 'ok') return '探路没通过：主机事实或能力可行性核对未过关，接入已就地拦住';
    var s = (step === 'preflight_remote') ? '远程可达性核对完成' : '主机实测完成';
    if (ft) {
      var bits = [];
      if (ft.distribution || ft.arch) bits.push((ft.distribution || ft.os || '') + ' / ' + (ft.arch || ''));
      if (ft.kernel) bits.push('内核 ' + ft.kernel);
      if (ft.disk_free_gb > 0) bits.push('磁盘可用 ' + ft.disk_free_gb.toFixed(0) + 'GB');
      if (bits.length) s += '：' + bits.join('，');
    }
    if (cnt) {
      s += '；能力可行性核对 ' + cnt.total + ' 项（通过 ' + cnt.ok
        + (cnt.fail ? '、未通过 ' + cnt.fail : '')
        + (cnt.delegated ? '、委派 Agent 侧 ' + cnt.delegated : '')
        + (cnt.unknown ? '、未核实 ' + cnt.unknown : '') + '）';
    }
    return s;
  }
  if (step === 'pick_version') {
    if (st === 'blocked') return '等你从兼容版本清单里选定一个 SAgent 版本';
    if (st === 'skipped') return '本次无需选版';
    if (st !== 'ok') return '版本未选定，安装不会开始';
    return 'SAgent 版本已选定：' + (obPlainTech(e.summary) || e.summary || '') + '，开始安装';
  }
  if (step === 'install_agent') {
    if (st === 'skipped') return '该机器已有 SAgent，跳过安装（复用现有 Agent）';
    if (st === 'running') return '正在分发安装包并启动 SAgent…';
    if (st !== 'ok') return 'SAgent 安装失败，接入已停止';
    return 'SAgent 已安装并启动，配置已下发，Agent 已注册回平台';
  }
  if (step === 'register_platform_device') {
    if (st === 'running') return '正在登记为平台设备…';
    if (st !== 'ok') return '平台设备登记失败';
    return '已登记为平台设备，并强制带上主机自监控';
  }
  if (step === 'self_metrics') {
    if (st === 'running') return '正在等 SAgent 发出第一次心跳…';
    if (st !== 'ok') return 'SAgent 心跳未到：主机自监控没生效';
    return 'SAgent 已存活并上报心跳，主机自监控生效——这是"这台机器活着"的唯一证据';
  }
  if (step === 'sync_config') {
    if (st === 'running') return '正在生成并下发采集配置…';
    if (st !== 'ok') return '采集配置下发失败';
    return '采集配置已生成并随心跳下发，配置版本 +1';
  }
  if (step === 'verify_probe') {
    if (st === 'running') return '正在真连一次采集目标并取回指标…';
    if (st !== 'ok') return '连通验证没通过：配置下发成功不等于真能采到';
    return '连通验证通过：探针真连一次并取回了一条指标';
  }
  if (step === 'observe_collect') {
    if (st === 'running') return '正在等第一份数据到达（平台直查 VictoriaMetrics）…';
    if (st !== 'ok') return '采集入库未确认：库里还没查到这台机器的序列';
    return '采集入库成功：指标已进入 VictoriaMetrics，接入闭环完成';
  }
  return e.summary || '';
}

// obOnbLogLines 把每步事件铺成终端日志行：大白话正文 + 技术细节尾巴；
// 失败步骤补「报错位置 + 报错内容 + 建议」，并就地给出重试入口
function obOnbLogLines(f, evs) {
  var lines = [];
  for (var i = 0; i < evs.length; i++) {
    var e = evs[i];
    if (!e || (e.status || 'pending') === 'pending') continue;   // 还没跑的步骤不进日志
    var st = e.status || 'pending';
    var dd = obDetail(e);
    var ts = obWzClock(e.started_at || e.finished_at || '');
    var cls = st === 'ok' ? 'ok' : (st === 'running' ? 'run' : (st === 'fail' ? 'fail' : (st === 'blocked' ? 'warn' : '')));
    var mark = st === 'ok' ? '✓' : (st === 'running' ? '◐' : (st === 'fail' ? '✕' : (st === 'blocked' ? '⏸' : (st === 'skipped' ? '–' : '·'))));
    var tx = '<b>' + mark + '</b> ' + obEscape(e.title || e.step) + ' — ' + obEscape(obOnbPlain(e.step, e, dd, f));
    if (st === 'ok' && OB_ONB_TECH_STEPS[e.step]) {
      var tech = obOnbTech(e, dd);
      if (tech) tx += ' <span class="det">（技术细节：' + obEscape(tech) + '）</span>';
    }
    var wn = obWzWarnN(e);
    if (wn) tx += ' <span class="ob-wz-warn">⚠ ' + obEscape(wn) + ' 项待复核（见下方明细）</span>';
    lines.push({ ts: ts, cls: cls, tx: tx });
    if (st === 'fail') {
      var bad = obLeadingDiag(dd);
      if (bad) lines.push({ ts: ts, cls: 'fail', tx: '&nbsp;&nbsp;↳ ' + (bad.code ? '[' + obEscape(bad.code) + '] ' : '') + obEscape(bad.message || '') });
      var pos = obWzFailPos(dd);
      if (pos) lines.push({ ts: ts, cls: 'fail', tx: '&nbsp;&nbsp;↳ 报错位置：<span class="pos">' + obEscape(pos) + '</span>' });
      if (bad && bad.hint) lines.push({ ts: ts, cls: 'warn', tx: '&nbsp;&nbsp;↳ 建议：' + obEscape(bad.hint) });
      lines.push({ ts: ts, cls: 'fail', action: e.step });
    }
  }
  if (f.status === 'running') lines.push({ cursor: true });
  return lines;
}

// ⑦ 最终入库校验：接入成功的判据不是"步骤都跑完了"，而是这四件事各自有据可查。
// 全部来自既有步骤的结论，展示层合成一个收尾节点，不新增探测链
function obOnbFinalCheck(f, byStep) {
  var specs = [
    { step: 'self_metrics', nm: 'SAgent 存活', desc: '主机自监控生效（心跳已回平台）' },
    { step: 'sync_config', nm: '配置下发', desc: '采集配置已生成并下发到 Agent' },
    { step: 'verify_probe', nm: '连通验证', desc: '真连一次并取回一条指标' },
    { step: 'observe_collect', nm: '样本入库', desc: '指标已进入 VictoriaMetrics' }
  ];
  var anyFail = false, allOk = true;
  var rows = [];
  for (var i = 0; i < specs.length; i++) {
    var e = byStep[specs[i].step];
    var st = e ? (e.status || 'pending') : 'pending';
    var ok = (st === 'ok' || st === 'skipped');
    if (st === 'fail') anyFail = true;
    if (!ok) allOk = false;
    var raw = '';
    if (st === 'skipped') raw = '不适用（已跳过）';
    else if (ok) raw = obOnbCheckRaw(specs[i].step, e);
    rows.push({ nm: specs[i].nm, desc: specs[i].desc, ok: ok, st: st, raw: raw });
  }
  var done = (f.status === 'done') && allOk;
  var h = '<div class="ob-wz-check' + (done ? ' pass' : '') + '">';
  h += '<div class="hd">🧪 最终入库校验（确认真的采到了）<span class="r">';
  if (done) h += '<span style="color:var(--success)">✓ 全部通过</span>';
  else if (anyFail) h += '<span style="color:var(--error)">✕ 未通过</span>';
  else h += '<span style="color:var(--muted)">待执行</span>';
  h += '</span></div>';
  for (var j = 0; j < rows.length; j++) {
    var c = rows[j];
    var cls = c.ok ? 'ok' : (anyFail ? 'bad' : '');
    var mark = c.ok ? '✓' : (anyFail ? '✕' : '·');
    h += '<div class="ob-wz-ck ' + cls + '"><span class="st">' + mark + '</span><span class="nm">' + c.nm + '</span>';
    h += '<span style="color:var(--muted)">' + obEscape(c.desc) + '</span>';
    h += '<span class="vv">' + obEscape(c.raw || '待执行') + '</span></div>';
  }
  if (done) h += '<div style="margin-top:8px;font-size:12px;color:#065f46;font-weight:600">✔ 四检全过：Agent 活着、配置已生效、探针通、样本已入库 —— 这台机器接入闭环完成。</div>';
  return h + '</div>';
}

// obOnbCheckRaw 校验行的机器口径证据（取不到就留空，不编造）
function obOnbCheckRaw(step, e) {
  var dd = e ? obDetail(e) : null;
  var data = dd && dd.data;
  if (step === 'self_metrics') return '心跳正常';
  if (step === 'sync_config') return '版本已 +1';
  if (step === 'verify_probe') {
    if (data && data.applied_version != null) return 'applied_version=' + data.applied_version;
    return '探针已取回指标';
  }
  if (step === 'observe_collect') {
    if (data && data.vm_series_count != null) return 'vm_series_count=' + data.vm_series_count;
    return '已入库';
  }
  return '';
}

// ⑧ 全量明细折叠屉：每步完整卡片（诊断/任务清单/尝试记录/原始输出/重试按钮）零删减。
// 已上提到决策闸门的人工决策 UI 在这里不再重复渲染（信息不丢失，闸门区已完整呈现）
function obOnbDetails(f, evs) {
  var h = '<div class="ob-wz-acc">';
  h += '<div class="h" onclick="obRowToggle(\'ob-onb-det\')"><span>🔍 全部环节执行明细与原始输出（排障用）</span><span class="ar" id="ob-onb-det-ar">▼ 明细</span></div>';
  h += '<div class="b" id="ob-onb-det" style="display:none">';
  for (var i = 0; i < evs.length; i++) {
    var e = evs[i];
    if (!e || (e.status || 'pending') === 'pending') continue;   // 还没跑到的步骤不进明细
    h += obStepCard(f, e, i, { noDecision: 1 });
  }
  return h + '</div></div>';
}

// ===================================================================
//  启停向导操作台（2026-09-24 用户评审：「启/停 详细界面的风格和展示效果 和 接入/卸载
//  不一致，调整和接入/卸载一致」）
//
//  启停与接入/卸载共用同一副八区骨架与视觉（目标对象 / 环节引导条 / 结论 / 影响范围核对 /
//  每步耗时 / 日志终端 / 启停校验 / 全量明细），差异只在内容：
//    ① 启停的"目标"不是一个采集范围，而是「对谁、做什么动作、影响什么」——动作与目标
//       在资源页行内按钮拉起时就已选定并留痕，这里只复述
//    ② 启停**没有人工闸门**（同日评审："停用，需要我确认？确认什么东西？？？"）：
//       ④ 区不摆确认按钮，只把预检实测与影响范围摊开留痕，并就地说明为何无需点头
//    ③ 启停的收尾校验不是"采到了没有"，而是"停干净了没有 / 起回来了没有"——
//       核对值来自执行 playbook 的自证行（SVC_STOPPED / SVC_STARTED / SVC_ALREADY …）
//  数据全部来自真实事件（timelineOf 的 status/duration_ms/detail/summary），不引入 fixtures。
// ===================================================================

// obSvcParams 本次启停的动作与目标。权威来源是 confirm_service 的 detail
// （动作与目标在拉起时选定、核对环节落库）；回落 service_execute；都没有就留空，
// 界面报「—」而不是编一个
function obSvcParams(byStep) {
  var order = ['confirm_service', 'service_execute'];
  for (var i = 0; i < order.length; i++) {
    var e = byStep[order[i]];
    var dd = e ? obDetail(e) : null;
    if (dd && dd.action) {
      return { action: dd.action, target: dd.target || '', label: dd.target_label || '',
               impact: dd.impact || '', auto: !!dd.auto_passed };
    }
  }
  return { action: '', target: '', label: '', impact: '', auto: false };
}

function obSvcActionLabel(a) {
  return { stop: '停止', start: '启动', restart: '重启' }[a] || (a || '启停');
}

// obSvcPluginInfo 解析 control.sock status 原文里的插件信息。
// 实测原文形如 {"plugins":{},"plugins_active":1,"uptime_seconds":…}——插件明细在 plugins 里，
// 但平铺形态「{插件名: 状态}」也认；解析不出就只报"不可用"（不编造插件清单）
function obSvcPluginInfo(raw) {
  var out = { names: [], active: null };
  if (!raw || raw === '-') return out;
  var o = null;
  try { o = JSON.parse(raw); } catch (e) { return out; }
  if (!o || typeof o !== 'object') return out;
  if (o.plugins && typeof o.plugins === 'object') {
    for (var k in o.plugins) out.names.push(k + (o.plugins[k] ? '·' + o.plugins[k] : ''));
  } else {
    var flat = false;
    for (var k2 in o) { if (typeof o[k2] === 'string') flat = true; }
    if (flat) { for (var k3 in o) out.names.push(k3 + '·' + o[k3]); }
  }
  if (o.plugins_active != null) out.active = o.plugins_active;
  return out;
}

// obSvcPre 目标机实测现状（preflight_service 的只读探测结论 pre={pid,guard,stopped,plugins_raw}）
function obSvcPre(byStep) {
  var e = byStep['preflight_service'];
  var dd = e ? obDetail(e) : null;
  var pre = (dd && dd.pre) || null;
  if (!pre) return null;
  var pi = obSvcPluginInfo(pre.plugins_raw);
  return { pid: pre.pid || '-', guard: !!pre.guard, stopped: !!pre.stopped,
           active: pi.active, names: pi.names };
}

// obSvcVerifyText 执行自证行的白话翻译（停=进程归零 / 启=pid 存活 / 幂等命中）
function obSvcVerifyText(line) {
  if (!line) return '';
  if (line.indexOf('SVC_ALREADY') >= 0) return '幂等命中：目标机状态已符合目标，未做任何改动';
  if (line.indexOf('SVC_PLUGIN_STOPPED') >= 0) return '目标插件已停止';
  if (line.indexOf('SVC_PLUGIN_STARTED') >= 0) return '目标插件已启动';
  if (line.indexOf('SVC_RESTARTED') >= 0) return '进程已重启（停与启在同一作业内原子完成，不留半死状态）';
  if (line.indexOf('SVC_STOPPED') >= 0) return '进程已停止（守护不会再把进程拉回）';
  if (line.indexOf('SVC_STARTED') >= 0) return '进程已启动并存活';
  if (line.indexOf('SVC_DIRTY') >= 0) return '自证核对未通过';
  return '';
}

// ① 目标对象 + 本次操作 + 目标机实测现状
function obSvcObject(f, evs, byStep) {
  var p = obSvcParams(byStep);
  var pre = obSvcPre(byStep);
  var h = '<div class="ob-wz-obj">';
  h += '<div class="who"><span class="lbl">目标对象</span><b>' + obEscape(f.resource_id) + '</b>';
  h += '<span class="ip">' + obEscape(f.resource_ip || '') + (f.ssh_ready ? ' · 平台持有 SSH 凭据' : ' · 未登记 SSH 凭据') + '</span></div>';
  h += '<div class="who"><span class="lbl">本次操作</span><b>' + obEscape(obSvcActionLabel(p.action) + ' ' + (p.label || p.target || '—')) + '</b>';
  h += '<span class="ip">' + obEscape(p.impact || '影响范围见下方「影响范围核对」') + '</span></div>';
  h += '<div class="pl"><span class="lbl">目标机实测现状（预检只读探测，未改动目标机）</span>';
  if (pre) {
    h += '<div class="ob-wz-chips">';
    h += '<span class="ob-wz-chip' + (pre.pid !== '-' ? ' hot' : '') + '"><span class="g"></span>进程 ' + (pre.pid !== '-' ? 'pid=' + obEscape(pre.pid) : '未在运行') + '</span>';
    h += '<span class="ob-wz-chip' + (pre.guard ? ' hot' : '') + '"><span class="g"></span>守护 run.sh ' + (pre.guard ? '在跑' : '未在跑') + '</span>';
    h += '<span class="ob-wz-chip' + (pre.stopped ? ' hot' : '') + '"><span class="g"></span>暂停标记 run/stopped ' + (pre.stopped ? '存在' : '无') + '</span>';
    var pl = pre.names.length ? pre.names.join('、') : (pre.active != null ? pre.active + ' 个' : '不可用');
    h += '<span class="ob-wz-chip"><span class="g"></span>采集插件 ' + obEscape(pl) + '</span>';
    h += '</div>';
  } else {
    h += '<div style="font-size:11px;color:var(--muted);margin-top:4px">'
      + (byStep['preflight_service'] ? '预检未取到目标机实测结论' : '待「启停前预检」完成后列出') + '</div>';
  }
  return h + '</div></div>';
}

// ② 核心环节引导条：启停的环节就是模板自己的四步（选择对象 → 启停前预检 → 核对影响 →
// 执行启停），照时间线实际下发的步骤出环，不写死清单；收尾补一个「启停校验」节点，
// 与卸载的「卸载校验」、接入的四段收口同构
function obSvcStepper(f, evs, byStep) {
  var h = '<div class="ob-wz-steps">';
  for (var i = 0; i < evs.length; i++) {
    var e = evs[i], st = e.status || 'pending';
    var cls = obWzCls(st);
    var mark = cls === 'ok' ? '✓' : (cls === 'fail' ? '✕' : (cls === 'wait' ? '!' : String(i + 1)));
    var tip = (e.title || e.step) + '\n' + (obDisplaySummary(e) || obDefaultSummary(st, e) || '');
    h += '<div class="ob-wz-st ' + cls + '" title="' + obEscape(tip) + '"><div class="ring">' + mark + '</div>'
      + '<div class="nm">' + obEscape(e.title || e.step) + '</div></div>';
    h += '<div class="ob-wz-lk' + (cls === 'ok' ? ' on' : '') + '"></div>';
  }
  var vs = obSvcVerifyState(byStep);
  h += '<div class="ob-wz-st ' + vs.cls + '" title="启停校验：预检实测有据 / 影响已留痕 / 执行自证通过">'
    + '<div class="ring">' + vs.mark + '</div><div class="nm">启停校验</div></div>';
  return h + '</div>';
}

// obSvcVerifyState 收尾节点状态：执行环节落终态才算校验出结论
function obSvcVerifyState(byStep) {
  var xe = byStep['service_execute'];
  var st = xe ? (xe.status || 'pending') : 'pending';
  if (st === 'fail') return { cls: 'fail', mark: '✕' };
  if (st === 'ok') return { cls: 'ok', mark: '✓' };
  if (st === 'running') return { cls: 'run', mark: '◐' };
  if (st === 'blocked') return { cls: 'wait', mark: '!' };
  return { cls: '', mark: '·' };
}

// ③ 结论横幅：一句话回答「到哪了、要不要我操作、成功没」
function obSvcConclusion(f, evs, byStep) {
  var p = obSvcParams(byStep);
  var act = obSvcActionLabel(p.action);
  var cls, icon, tt, sub;
  if (f.status === 'done') {
    cls = 'ok'; icon = '✓';
    if (p.action === 'stop') {
      tt = '已停止 · 采集中断（平台标维护态）';
      sub = 'SAgent 已按守护协议停稳，不会被 3 秒拉回；心跳消失是维护态的预期表现，不算失联。需要恢复时在资源页点「启动」即可。';
    } else {
      tt = '已启动 · 采集已恢复';
      sub = 'SAgent 已存活并上报心跳，心跳与数据自行回来。本次操作全程有据可查（预检实测 + 执行自证）。';
    }
  } else if (f.status === 'failed') {
    cls = 'fail'; icon = '✕'; tt = act + '失败 · 需要你介入排查';
    sub = '流程在失败环节停止，未继续执行后续步骤。下方日志已标出报错位置与内容，可在原地重试该步。';
  } else if (f.status === 'cancelled' || f.status === 'canceled') {
    cls = 'wait'; icon = '–'; tt = act + '已取消 · 目标机未做进一步改动';
    sub = '本次启停已取消。如需重新执行，在资源页对该主机点「启动」或「停用」。';
  } else if (f.status === 'blocked') {
    cls = 'wait'; icon = '!'; tt = '旧版启停闸门（已移除）· 当时卡在这里没往下走';
    sub = '这是闸门移除前落库的历史记录：那时启停要人工点头，而界面并没有确认入口，流程卡在半路、执行环节从未运行。现在启停改为「核对通过即自动放行」，不再需要人工确认。';
  } else if (f.status === 'running') {
    cls = 'run'; icon = '◐'; tt = act + '进行中 · 不需要你操作';
    sub = '平台 ansible 正在目标机执行，完成后自动回报；界面每 ' + (OB_FLOW_POLL_MS / 1000) + ' 秒自动刷新。失败即停并给出原因。';
  } else {
    cls = 'wait'; icon = '⏸'; tt = act + '待开始'; sub = '流水线尚未开始推进。';
  }
  var h = '<div class="ob-wz-concl ' + cls + '"><span class="dot">' + icon + '</span><div>';
  h += '<div class="tt">' + obEscape(tt) + '</div>';
  h += '<div class="sub">' + obEscape(sub) + '</div>';
  return h + '</div></div>';
}

// ④ 影响范围核对（启停**没有人工闸门**，这一区不摆确认按钮）。
// 与卸载的「卸载范围二次确认」、接入的「版本选定」同处一栏，但职责不同：
// 那两处是真闸门（不可逆动作 / 人工决策点），启停这里只是把「做什么、影响什么、
// 幂等命中会怎样」摊开留痕，并就地说明为何不需要人点头
function obSvcImpact(f, byStep) {
  var e = byStep['confirm_service'];
  var st = e ? (e.status || 'pending') : 'pending';
  var p = obSvcParams(byStep);
  var pre = obSvcPre(byStep);
  var right = st === 'ok' ? '已核对' : (st === 'fail' ? '未通过' : (st === 'blocked' ? '旧版闸门' : (st === 'running' ? '核对中' : '待预检')));
  var h = '<div class="ob-wz-card"><div class="chd">⏻ 影响范围核对<span class="r">' + obEscape(right) + '</span></div>';
  if (st === 'ok') {
    h += '<div style="font-size:12px;line-height:1.7">';
    h += '<div>· 动作：<b>' + obEscape(obSvcActionLabel(p.action) + ' ' + (p.label || p.target || '—')) + '</b></div>';
    h += '<div>· 影响：' + obEscape(p.impact || '—') + '</div>';
    if (pre && p.action === 'stop' && pre.pid === '-') {
      h += '<div>· 幂等：目标机 SAgent 未在运行，本次停止将<b>幂等命中</b>——如实报告且不做改动</div>';
    } else if (pre && p.action !== 'stop' && pre.pid !== '-') {
      h += '<div>· 幂等：进程已在运行，本次启动将<b>幂等命中</b>——如实报告且不做改动</div>';
    }
    h += '<div style="margin-top:6px;color:var(--success)">✓ 核对通过，已自动放行到执行环节（启停不设人工确认）</div>';
    h += '</div>';
    h += '<div style="font-size:11px;color:var(--muted);margin-top:6px">为什么不用你点头：启停是可逆轻动作（停→启随时可回），动作与目标在资源页点按钮时就已选定并留痕审计，平台核对预检实测通过后直接执行。</div>';
  } else if (st === 'fail') {
    h += '<div style="font-size:12px;color:var(--error)">✕ ' + obEscape(e.summary || '核对未通过，启停已就地拦住（未触碰目标机）') + '</div>';
  } else if (st === 'blocked') {
    h += '<div style="font-size:12px;color:var(--warn)">⏸ 旧版启停闸门（已移除）——这条流水线当时停在这里等人工确认，而界面并无确认入口，因此执行环节从未运行。现在改为「核对通过即自动放行」。</div>';
  } else {
    h += '<div style="font-size:12px;color:var(--muted)">预检只读探测完成后，这里会摊开本次要做什么、影响什么、幂等命中会怎样——核对通过即自动放行，不需要你确认。</div>';
  }
  return h + '</div>';
}

// ⑤ 每步耗时与进度：与接入/卸载共用同一张表（步骤清单取时间线，不写死）
function obSvcDuration(f, evs) { return obOnbDuration(f, evs); }

// ⑥ 实时执行日志终端
function obSvcTerminal(f, evs) { return obTermShell(f, obSvcLogLines(f, evs), obSvcTermTitle(f)); }

function obSvcTermTitle(f) {
  var who = f.resource_ip ? ('deploy@' + f.resource_ip) : f.resource_id;
  if (f.status === 'done') return 'ansible-playbook agent_service.yml — ' + who + ' · ✓ 执行自证通过';
  if (f.status === 'failed') return 'ansible-playbook agent_service.yml — ' + who + ' · ✕ 执行中断';
  return 'ansible-playbook agent_service.yml — ' + who;
}

// obSvcTech 启停日志的「技术细节」尾巴：执行自证行（SVC_*）比提纯后的正文更有排障价值
function obSvcTech(e, dd) {
  if (dd && dd.verify_line) return dd.verify_line;
  return obPlainTech(e && e.summary);
}

// obSvcPlain 把执行器原文翻成运维一眼能懂的大白话（与卸载/接入同一套门槛）。
// 门槛低 ≠ 信息少：核对值以「技术细节」尾巴附在同一条日志上，完整原文仍在下方明细里
function obSvcPlain(step, e, dd, f) {
  var st = e.status || 'pending';
  if (step === 'pick_object') {
    return '已锁定目标机器 ' + ((f && (f.resource_ip || f.resource_id)) || '') + '，开始启停';
  }
  if (step === 'preflight_service') {
    if (st === 'running') return '正在只读探测目标机：SAgent 进程 / 守护形态 / 暂停标记 / 插件运行状态…';
    if (st === 'blocked') return '等待预检实测结果';
    if (st !== 'ok') return '预检没通过：目标机状态没取到，启停已就地拦住（本环节未触碰目标机）';
    var pre = dd && dd.pre;
    if (!pre) return '目标机实测完成';
    var bits = [];
    bits.push('进程 ' + (pre.pid && pre.pid !== '-' ? 'pid=' + pre.pid : '未在运行'));
    bits.push('守护 ' + (pre.guard ? '在跑' : '未在跑'));
    bits.push('暂停标记 ' + (pre.stopped ? '存在' : '无'));
    return '目标机实测完成：' + bits.join('，') + '（本环节未改动目标机任何东西）';
  }
  if (step === 'confirm_service') {
    if (st === 'blocked') return '旧版启停闸门（已移除）——当时卡在这里等人工点头，现已改为「核对通过即自动放行」';
    if (st === 'running') return '正在核对预检实测与影响范围…';
    if (st !== 'ok') return '核对未通过，启停已拦住：' + (e.summary || '');
    var s = '已核对本次操作：' + obSvcActionLabel(dd && dd.action) + ' ' + ((dd && dd.target_label) || '');
    if (dd && dd.impact) s += '，影响：' + dd.impact;
    if (dd && dd.auto_passed) s += '；核对通过，自动放行到执行（启停不设人工确认）';
    return s;
  }
  if (step === 'service_execute') {
    if (st === 'blocked') return '旧版启停闸门（已移除）——当时被卡住，本环节从未执行';
    if (st === 'running') return '平台 ansible 正在执行启停（逐任务回报）…';
    if (st !== 'ok') return '启停执行失败：' + (e.summary || '');
    var vt = obSvcVerifyText(dd && dd.verify_line);
    return '启停执行完成' + (vt ? '：' + vt : '，并完成自证核对');
  }
  return e.summary || '';
}

// obSvcLogLines 把每步事件铺成终端日志行（结构与 obOnbLogLines 一致）
function obSvcLogLines(f, evs) {
  var lines = [];
  for (var i = 0; i < evs.length; i++) {
    var e = evs[i];
    if (!e || (e.status || 'pending') === 'pending') continue;   // 还没跑的步骤不进日志
    var st = e.status || 'pending';
    var dd = obDetail(e);
    var ts = obWzClock(e.started_at || e.finished_at || e.created_at || '');
    var cls = st === 'ok' ? 'ok' : (st === 'running' ? 'run' : (st === 'fail' ? 'fail' : (st === 'blocked' ? 'warn' : '')));
    var mark = st === 'ok' ? '✓' : (st === 'running' ? '◐' : (st === 'fail' ? '✕' : (st === 'blocked' ? '⏸' : (st === 'skipped' ? '–' : '·'))));
    var tx = '<b>' + mark + '</b> ' + obEscape(e.title || e.step) + ' — ' + obEscape(obSvcPlain(e.step, e, dd, f));
    if (st === 'ok' && (e.step === 'preflight_service' || e.step === 'service_execute')) {
      var tech = obSvcTech(e, dd);
      if (tech) tx += ' <span class="det">（技术细节：' + obEscape(tech) + '）</span>';
    }
    var wn = obWzWarnN(e);
    if (wn) tx += ' <span class="ob-wz-warn">⚠ ' + obEscape(wn) + ' 项待复核（见下方明细）</span>';
    lines.push({ ts: ts, cls: cls, tx: tx });
    if (st === 'fail') {
      var bad = obLeadingDiag(dd);
      if (bad) lines.push({ ts: ts, cls: 'fail', tx: '&nbsp;&nbsp;↳ ' + (bad.code ? '[' + obEscape(bad.code) + '] ' : '') + obEscape(bad.message || '') });
      var pos = obWzFailPos(dd);
      if (pos) lines.push({ ts: ts, cls: 'fail', tx: '&nbsp;&nbsp;↳ 报错位置：<span class="pos">' + obEscape(pos) + '</span>' });
      if (bad && bad.hint) lines.push({ ts: ts, cls: 'warn', tx: '&nbsp;&nbsp;↳ 建议：' + obEscape(bad.hint) });
      lines.push({ ts: ts, cls: 'fail', action: e.step });
    }
  }
  if (f.status === 'running') lines.push({ cursor: true });
  return lines;
}

// ⑦ 启停校验：启停成功的判据不是"步骤都跑完了"，而是这三件事各自有据可查
// （预检实测有据 / 影响已留痕 / 执行自证通过）。全部来自既有步骤的结论，
// 展示层合成一个收尾节点，不新增探测链
function obSvcFinalCheck(f, byStep) {
  var p = obSvcParams(byStep);
  var pre = obSvcPre(byStep);
  var pe = byStep['preflight_service'], ce = byStep['confirm_service'], xe = byStep['service_execute'];
  var xd = xe ? obDetail(xe) : null;
  var checks = [
    { nm: '目标机实测', desc: '预检只读探测：进程 / 守护 / 暂停标记',
      ok: !!pre && (pe && pe.status || '') === 'ok',
      raw: pre ? ('pid=' + pre.pid + ' guard=' + (pre.guard ? 'yes' : 'no') + ' stopped=' + (pre.stopped ? 'yes' : 'no')) : '' },
    { nm: '影响已核对', desc: '动作与影响范围摊开留痕（无需人工确认）',
      ok: (ce && (ce.status || '') === 'ok'),
      raw: (ce && (ce.status || '') === 'ok') ? (obSvcActionLabel(p.action) + ' ' + (p.label || p.target || '')) : '' },
    { nm: '执行自证', desc: '停=进程归零 / 启=pid 存活（执行 playbook 的核对行）',
      ok: (xe && (xe.status || '') === 'ok'),
      raw: (xd && xd.verify_line) || '' }
  ];
  var anyFail = false, allOk = true;
  for (var q = 0; q < checks.length; q++) { if (!checks[q].ok) allOk = false; }
  var failSteps = ['preflight_service', 'confirm_service', 'service_execute'];
  for (var w = 0; w < failSteps.length; w++) {
    var we = byStep[failSteps[w]];
    if (we && (we.status || '') === 'fail') anyFail = true;
  }
  var done = (f.status === 'done') && allOk;
  var h = '<div class="ob-wz-check' + (done ? ' pass' : '') + '">';
  h += '<div class="hd">🧪 启停校验（确认真的停干净了 / 起回来了）<span class="r">';
  if (done) h += '<span style="color:var(--success)">✓ 全部通过</span>';
  else if (anyFail) h += '<span style="color:var(--error)">✕ 未通过</span>';
  else h += '<span style="color:var(--muted)">待执行</span>';
  h += '</span></div>';
  for (var j = 0; j < checks.length; j++) {
    var c = checks[j];
    var cls = c.ok ? 'ok' : (anyFail ? 'bad' : '');
    var mark = c.ok ? '✓' : (anyFail ? '✕' : '·');
    h += '<div class="ob-wz-ck ' + cls + '"><span class="st">' + mark + '</span><span class="nm">' + c.nm + '</span>';
    h += '<span style="color:var(--muted)">' + obEscape(c.desc) + '</span>';
    h += '<span class="vv">' + obEscape(c.raw || '待执行') + '</span></div>';
  }
  if (done) {
    var tail = (p.action === 'stop')
      ? '✔ 三检全过：预检有据、影响已留痕、进程已归零 —— 这台机器的 SAgent 已停稳（维护态），心跳消失属预期。'
      : '✔ 三检全过：预检有据、影响已留痕、进程已存活 —— 这台机器的采集已恢复。';
    h += '<div style="margin-top:8px;font-size:12px;color:#065f46;font-weight:600">' + tail + '</div>';
  }
  return h + '</div>';
}

// ⑧ 全量明细折叠屉：每步完整卡片（诊断/任务清单/尝试记录/原始输出/重试按钮）零删减
function obSvcDetails(f, evs) {
  var h = '<div class="ob-wz-acc">';
  h += '<div class="h" onclick="obRowToggle(\'ob-svc-det\')"><span>🔍 全部环节执行明细与原始输出（排障用）</span><span class="ar" id="ob-svc-det-ar">▼ 明细</span></div>';
  h += '<div class="b" id="ob-svc-det" style="display:none">';
  for (var i = 0; i < evs.length; i++) {
    var e = evs[i];
    if (!e || (e.status || 'pending') === 'pending') continue;   // 还没跑到的步骤不进明细
    h += obStepCard(f, e, i);
  }
  return h + '</div></div>';
}

// obStepCard 单个步骤卡片：从 obFlowBody 的时间线循环抽出（五环节块体内复用）。
// 内容零删减——连接目标/超时栏/诊断/任务清单/尝试记录/原始输出/重试按钮全在这里。
// opts.noDecision：接入向导的明细屉用——人工决策 UI（选版清单 / 探路回报执行包）
// 已上提到④决策闸门就地呈现，明细里不再重复一份（信息不丢，只是不摆两处）
function obStepCard(f, e, i, opts) {
  e = obLegacyServiceGate(e);
  var noDecision = !!(opts && opts.noDecision);
  var st = e.status || 'pending';
  var h = '<div class="ob-step ' + st + '">';
    h += '<div class="ic">' + (OB_STEP_ICON[st] || '○') + '</div>';
    h += '<div class="hd">' + obEscape(e.title || e.step) + ' ' + obScope(e.scope);
    if (e.duration_ms) h += '<span style="color:var(--muted);font-weight:400;font-size:11px">' + obDur(e.duration_ms) + '</span>';
    h += '</div>';
    var metaTxt = obDisplaySummary(e) || obDefaultSummary(st, e);
    h += '<div class="meta">' + obEscape(metaTxt) + '</div>';

    var dd = obDetail(e);
    // 结论条置顶：✓通过（带关键信息）/ ✕未通过（带原因+下一步）/ ⏸等你操作——
    // 技术细节沉到下方的诊断与原始输出里
    h += obConclusion(st, e, dd);

    // 连接目标 + 分级诊断 + 自动执行回落原因：一律直铺在卡片上。
    // 异常必须「进门就看见」，不能藏在「查看原始数据」后面——那是本次改造的出发点
    if (dd) {
      h += obConnLine(dd.conn);
      h += obTimeoutBar(e.timeout_view, dd);
      h += obDiagBox(dd.diagnosis, 'ob-dg-' + i, st === 'fail' ? obLeadingDiag(dd) : null);
      // 兼容老事件：没有 diagnosis 时才单独渲染 ansible_fallback，避免同一告警出现两条
      if (dd.ansible_fallback && !obHasDiag(dd.diagnosis, 'ansible_fallback')) {
        h += obDiagBox([{ level: 'warn', code: 'ansible_fallback', message: dd.ansible_fallback,
          hint: '在资源台账补齐该资源的 SSH 凭据后，点「↻ 重试该步」即可改为平台自动执行' }]);
      }
      // 第二层·关键证据：尝试历史（重试了几次）+ 机器档案 + 在跑的进度——留在卡片上
      h += obAttemptList(dd.attempts);
      // 主机资源信息卡：探路采到的机器档案（结构化、默认收起、已同步资源台账）
      h += obFactsCard(dd.facts, i);
      // 任务在跑但还没有终态任务结果时，至少让运维看到"平台在跑、跑到哪了"
      if (st === 'running' && dd.progress) h += obProgressLine(dd.progress);
    }

    if (e.detail) {
      var evText = obEvidenceText(dd);
      // 第三层·执行明细与原文：逐任务输出 / 阶段记录 / PLAY RECAP 统一收进抽屉——
      // 成功路径默认只露结论与关键证据；失败时抽屉自动展开（见下方 display 规则），
      // 排障依旧一次点击都不用多点（2026-09-22 用户评审：信息分层）
      var deepText = (dd ? obPhaseList(dd.phases) + obTaskList(dd.tasks) : '') +
        (dd && dd.recap_line ? '<div class="ob-conn">PLAY RECAP <code>' + obEscape(dd.recap_line) + '</code></div>' : '');
      h += '<div style="margin-top:6px"><button class="btn btn-o btn-sm" onclick="obToggleDet(' + i + ')">' +
        (deepText ? '执行明细与原文' : (evText ? '原始输出（ansible 原文）' : '查看原始数据')) + '</button>';
      if (evText) h += ' <button class="btn btn-o btn-sm" onclick="obCopyText(\'ob-det-' + i + '\')">复制原文</button>';
      h += '</div>';
      // 失败即展开：既然已经出错，就别再让运维多点一次
      h += '<div class="ob-det" id="ob-det-' + i + '" style="display:' + (st === 'fail' ? 'block' : 'none') + '">' +
        deepText + obEscape(evText || obPretty(e.detail)) + '</div>';
    }
    // 只有 fail 才可重试/强制（2026-09-22 用户评审）：blocked 是停等——等人工决策或
    // 等上游实测，没有"可跳过的工作"；曾可对等待中的下游步骤 force 造出假 OK、
    // 绕过卸载确认门禁（后端已同步封堵）。停等环节的出路只有一个：去上游完成决策
    if (st === 'fail') {
      h += '<div style="margin-top:6px"><button class="btn btn-o btn-sm" onclick="obRetry(' + f.id + ',\'' + obEscape(e.step) + '\')">↻ 重试该步</button>';
      h += ' <button class="btn btn-d btn-sm" onclick="obForce(' + f.id + ',\'' + obEscape(e.step) + '\')">强制继续（需留痕）</button></div>';
    }
    // external 步骤在跑：平台 ansible 代执行的渲染「自动执行中」面板；
    // 旧路径（无凭据兜底）才渲染执行包 + 结构化回报表单。
    // 「回报结果」走 report 通道（带证据、审计可区分），「强制通过」是无证据时的兜底
    if (st === 'running' && e.scope === 'external') {
      var d = obDetail(e);
      if (d && d.executor === 'ansible') {
        // 平台 ansible 代执行：全自动，完成后自动回报——人工回报/强制通过按钮不出现
        // （2026-09-22 用户评审：这两个按钮是人工兜底路径的产物，混进 ansible
        //  执行中面板会让人误以为需要人工确认）
        h += '<div style="margin:8px 0;padding:8px 10px;border:1px solid var(--border);border-radius:6px;background:var(--bg)">';
        h += '<div style="font-size:12px">🤖 <b>平台 ansible 执行中</b> · <code style="font-size:11px">' + obEscape(d.target || '') + '</code></div>';
        h += '<div style="font-size:11px;color:var(--muted);margin-top:3px">' + obEscape(d.note || '完成后自动回报，无需人工操作') + '；点「↻ 刷新」查看最新进展，原始输出在「查看原始数据」。</div>';
        h += '</div>';
      } else if (d && d.instruction && !noDecision) {
        // 旧路径（无凭据兜底）：执行包交给人，跑完手动回报。
        // 「回报结果」走 report 通道（带证据、审计可区分），「强制通过」是无证据时的兜底。
        // 接入向导里这套已上提到④决策闸门（「主机探路回报」），明细屉用 noDecision 抑制
        h += obExecutorBox(f, d, i, e.step);
        h += '<div style="margin-top:6px"><button class="btn btn-d btn-sm" onclick="obReport(' + f.id + ',\'' + obEscape(e.step) + '\')">回报结果（带证据留痕）</button>';
        h += ' <button class="btn btn-o btn-sm" style="color:#b91c1c;border-color:#b91c1c" title="无证据时的例外通道，审计标记 forced" onclick="obForce(' + f.id + ',\'' + obEscape(e.step) + '\')">强制通过</button></div>';
      }
    }
    // pick_version 停等人工：渲染兼容版本清单，人工点选确认（版本决策留痕到人）。
    // 接入向导的明细屉抑制这一份——清单已在④决策闸门就地给出
    if (st === 'blocked' && e.step === 'pick_version' && !noDecision) {
      var dv = obDetail(e);
      if (dv && dv.waiting_for === 'human_pick' && dv.candidates && dv.candidates.length) {
        h += obVersionPicker(f, dv);
      } else if (dv && dv.waiting_for === 'probe') {
        h += '<div style="margin:8px 0;padding:8px 10px;border-radius:6px;background:#fef3c7;color:#92400e;font-size:12px">⏸ 等待主机探路实测 OS/架构。探路结果回填后本环节自动出兼容清单，由人工选定版本——不再使用 latest 兜底。</div>';
      }
    }
    // 卸载前的人工确认：把实测到的插件与自愈来源摊开给操作人员，人点头才继续。
    // 这不是装饰性弹窗——内容全部来自上一步的目标机实测结果
    if (st === 'blocked' && e.step === 'confirm_uninstall') {
      var dcf = obDetail(e);
      if (dcf && dcf.waiting_for === 'human_confirm_offboard') h += obOffboardConfirm(f, dcf);
      else if (dcf && dcf.waiting_for === 'scan') h += '<div style="margin:8px 0;padding:8px 10px;border-radius:6px;background:#fef3c7;color:#92400e;font-size:12px">⏸ 等待「扫描采集插件与自愈来源」完成。扫描结果出来后本环节自动列出影响范围，供人工确认——不使用任何默认假设。</div>';
    }
    // 卸载前置三件套的结论面板：扫描清单 / 插件停没停 / 自启清没清，各自摊开
    if ((st === 'ok' || st === 'fail') && OB_OFFBOARD_PANELS[e.step]) {
      h += obOffboardOutcome(e.step, st, obDetail(e));
    }
    h += '</div>';
  return h;
}

function obScope(s) {
  var m = { platform: '平台', agent: 'Agent', external: '外部执行器' };
  if (!s) return '';
  return '<span class="badge b-r" style="font-weight:400">' + (m[s] || s) + '</span>';
}

function obDetail(e) {
  if (!e.detail) return null;
  if (typeof e.detail === 'object') return e.detail;
  try { return JSON.parse(e.detail); } catch (err) { return null; }
}

// obTimeoutBadge 列表页超时徽标（剩余时间 / 是否已超期）
function obTimeoutBadge(tv) {
  if (!tv || tv.remain_sec == null) return '';
  if (tv.overdue) {
    return ' <span class="ob-warn" style="background:#fee2e2;border-color:#fca5a5;color:#991b1b" title="已超过超时上限，平台正在处置（自动重试或判失败）">⏱ 已超时</span>';
  }
  return ' <span style="font-size:11px;color:var(--muted)" title="超时上限：总时长 ' + tv.run_limit_sec + 's / 无进展 ' + tv.idle_limit_sec + 's' + (tv.auto_retry ? '；超时会自动重试 ' + (tv.max_attempts - 1) + ' 次' : '；超时不会自动重试') + '">⏱ 剩余 ' + tv.remain_sec + 's</span>';
}

// obTimeoutBar 步骤卡片上的超时窗口：已运行多久、还剩多久、第几次尝试、超时后平台会怎么做。
// 「不能无底线等待」必须让人看得见，否则运维只能干等
function obTimeoutBar(tv, dd) {
  if (!tv) return '';
  var lim = '总时长上限 ' + tv.run_limit_sec + 's · 无进展上限 ' + tv.idle_limit_sec + 's';
  var att = '';
  if (tv.max_attempts > 1) {
    att = ' · 第 ' + tv.attempt + '/' + tv.max_attempts + ' 次尝试';
    att += tv.auto_retry ? '（超时会自动重试 ' + (tv.max_attempts - 1) + ' 次，之后判失败需人工介入）' : '';
  } else {
    att = ' · 超时不自动重试，直接判失败需人工介入';
  }
  var managed = tv.managed ? '平台自动执行' : '按人工执行节奏计（上限 ' + tv.run_limit_sec + 's）';
  if (tv.overdue) {
    return '<div class="ob-timeout over">⏱ <b>已超过超时上限</b>（已运行 ' + tv.elapsed_sec + 's / 静默 ' + tv.idle_sec + 's，' + obEscape(lim) + '）。平台正在处置：先自动重试，重试用尽后判失败并给出排查建议。</div>';
  }
  return '<div class="ob-timeout run">⏱ 已运行 <b>' + tv.elapsed_sec + 's</b> · 剩余 <b>' + tv.remain_sec + 's</b>（' +
    obEscape(lim) + '；当前由' + obEscape(tv.remain_by === 'idle' ? '「无进展」上限决定' : '「总时长」上限决定') + '）' +
    obEscape(att) + ' · ' + obEscape(managed) + '</div>';
}

// obProgressLine 只有进度快照（还没有终态任务结果）时的简版提示
function obProgressLine(pr) {
  if (!pr) return '';
  var h = '<div class="ob-conn">执行进度：' + (pr.tasks_done || 0) + ' 个任务完成';
  if (pr.current_task) h += ' · 当前 <code>' + obEscape(pr.current_task) + '</code>';
  if (pr.phase) h += ' · 阶段 ' + obEscape(obPhaseLabel(pr.phase));
  h += ' · 最后活动 ' + obEscape(pr.last_activity_at || '') + '</div>';
  return h;
}

// obTaskList 执行过程逐任务清单——「安装过程中所有信息回到界面」的主体。
// 每个任务：序号、状态、名称、耗时、loop 子项、失败原因/输出；失败任务整行高亮
function obTaskList(tasks) {
  if (!tasks || !tasks.length) return '';
  var failed = 0;
  for (var i = 0; i < tasks.length; i++) {
    var stt = tasks[i] && tasks[i].status;
    if (stt === 'failed' || stt === 'unreachable') failed++;
  }
  var h = '<div class="ob-tasks"><div class="ob-tasks-hd">🧩 执行过程 · 共 ' + tasks.length + ' 个任务' +
    (failed ? ' · <span style="color:var(--error);font-weight:600">' + failed + ' 个失败</span>' : '') + '</div>';
  for (var j = 0; j < tasks.length; j++) {
    var tk = tasks[j] || {};
    var st = tk.status || 'unknown';
    var bad = (st === 'failed' || st === 'unreachable');
    h += '<div class="ob-task ' + obEscape(st) + '">';
    h += '<span class="idx">' + (tk.index || (j + 1)) + '</span>';
    h += '<span class="st ' + obEscape(st) + '">' + obEscape(obTaskLabel(st)) + '</span>';
    h += '<span class="nm">' + (tk.phase ? '[' + obEscape(obPhaseLabel(tk.phase)) + '] ' : '') + obEscape(tk.name || '(未命名任务)') + '</span>';
    if (tk.duration_ms) h += '<span class="dur">' + obDur(tk.duration_ms) + '</span>';
    if (tk.changed) h += '<span class="dur" title="该任务改动了目标机状态">已改动</span>';
    h += '</div>';
    if (tk.items && tk.items.length) h += '<div class="ob-task-sub">受影响子项（' + tk.items.length + '）：' + obEscape(tk.items.join(' , ')) + '</div>';
    if (tk.msg) h += '<div class="ob-task-sub' + (bad ? ' err' : '') + '">msg：' + obEscape(tk.msg) + '</div>';
    if (tk.stdout) h += '<div class="ob-task-sub">stdout：' + obEscape(obClip(tk.stdout, 600)) + '</div>';
    if (tk.stderr) h += '<div class="ob-task-sub err">stderr：' + obEscape(obClip(tk.stderr, 600)) + '</div>';
    if (tk.rc !== undefined && tk.rc !== null) h += '<div class="ob-task-sub">rc = ' + obEscape(String(tk.rc)) + '</div>';
  }
  return h + '</div>';
}

// obPhaseList 阶段记录：语法校验 → playbook → 采集目标登记，各自结果一眼可见
function obPhaseList(phases) {
  if (!phases || !phases.length) return '';
  var parts = [];
  for (var i = 0; i < phases.length; i++) {
    var p = phases[i] || {};
    var s = p.phase === 'syntax' ? '语法校验' : (p.phase === 'playbook' ? '安装 playbook' : (p.phase === 'register' ? '采集目标登记' : p.phase));
    var v = p.status || (p.rc !== undefined ? ('rc=' + p.rc) : '—');
    if (p.timed_out) v = '超时终止';
    if (p.tasks) v += ' · ' + p.tasks + ' 个任务';
    if (p.recap) v += '（' + p.recap + '）';
    parts.push(obEscape(s) + ' <b>' + obEscape(String(v)) + '</b>');
  }
  return '<div class="ob-conn">阶段：' + parts.join(' → ') + '</div>';
}

// obAttemptList 尝试历史：第几次、跑了多久、怎么结束的。
// 超时自动重试是平台自己救的火，运维必须能看到救过几次
function obAttemptList(ats) {
  if (!ats || !ats.length) return '';
  var h = '<div class="ob-attempts">尝试记录：';
  for (var i = 0; i < ats.length; i++) {
    var a = ats[i] || {};
    var res = a.result === 'ok' ? '成功' : (a.result === 'fail' ? '失败' :
      (a.result === 'timeout_idle' ? '超时（无响应）' : (a.result === 'timeout_run' ? '超时（总时长）' : (a.result || '—'))));
    h += '<div>· 第 ' + obEscape(String(a.no || (i + 1))) + ' 次：<code>' + obEscape(a.started_at || '') + '</code> → <code>' + obEscape(a.ended_at || '') +
      '</code> · ' + obEscape(res) + (a.waited_sec ? '（等待 ' + a.waited_sec + 's）' : '') + (a.note ? ' · ' + obEscape(a.note) : '') + '</div>';
  }
  return h + '</div>';
}

function obTaskLabel(s) {
  var m = { ok: '成功', changed: '已变更', failed: '失败', unreachable: '不可达', skipping: '跳过', unknown: '未知' };
  return m[s] || s || '未知';
}

function obPhaseLabel(p) {
  var m = { ping: '联通性测试', port_check: '端口核对', setup: '事实采集', df: '磁盘兜底采集', env_check: '环境预检', syntax: '语法校验', playbook: '安装 playbook', register: '采集目标登记' };
  return m[p] || p || '';
}

// obClip 长文本裁剪（界面只给一段，全文在「原始输出」里）
function obClip(s, n) {
  s = String(s == null ? '' : s);
  if (s.length <= n) return s;
  return s.slice(0, n) + ' …（共 ' + s.length + ' 字符，完整内容见「原始输出」）';
}

// obDiagBox 分级诊断条：fatal 红 / warn 黄 / info 蓝。
// 平台侧对「异常」的分级约定：fatal 阻断流程、warn 不阻断但必须给人看（端口来源可疑、
// facts 缺字段、自动执行回落都属于这一级）、info 只是"核对过了"的正面证据
// skip：结论条已经原样引用过的那条诊断——同一句话不在卡片上出现两次
function obDiagBox(dgs, key, skip) {
  if (!dgs || !dgs.length) return '';
  // 分层（2026-09-22 用户拍板）：⚠/✕ 需要人处理 → 直接铺出来；ℹ 是排障细节 →
  // 默认收进「技术细节」，聚焦关键信息
  var vis = [], infos = [];
  for (var i = 0; i < dgs.length; i++) {
    var d = dgs[i] || {};
    if (d === skip) continue;
    var lv = d.level === 'fatal' ? 'fatal' : (d.level === 'info' ? 'info' : 'warn');
    if (lv === 'info') infos.push(d); else vis.push({ lv: lv, d: d });
  }
  var h = '';
  for (var v = 0; v < vis.length; v++) {
    var lv2 = vis[v].lv, d2 = vis[v].d;
    var ic = lv2 === 'fatal' ? '✕' : '⚠';
    h += '<div class="ob-diag ' + lv2 + '">' + ic + ' <b>' + obEscape(d2.code || lv2) + '</b> · ' + obEscape(d2.message || '');
    if (d2.hint) h += '<div class="hint">处理建议：' + obEscape(d2.hint) + '</div>';
    h += '</div>';
  }
  if (infos.length) {
    var id = (key || 'ob-dg') + '-infos';
    h += '<div style="margin-top:6px"><button class="btn btn-o btn-sm" onclick="obToggleAny(\'' + id + '\')">技术细节（' + infos.length + ' 条排障信息）</button></div>';
    h += '<div id="' + id + '" style="display:none">';
    for (var j = 0; j < infos.length; j++) {
      var d3 = infos[j];
      h += '<div class="ob-diag info">ℹ <b>' + obEscape(d3.code || 'info') + '</b> · ' + obEscape(d3.message || '');
      if (d3.hint) h += '<div class="hint">处理建议：' + obEscape(d3.hint) + '</div>';
      h += '</div>';
    }
    h += '</div>';
  }
  return h;
}

// obHasDiag 诊断数组里是否已含某 code（避免同一异常渲染两条）
function obHasDiag(dgs, code) {
  if (!dgs || !dgs.length) return false;
  for (var i = 0; i < dgs.length; i++) {
    if (dgs[i] && dgs[i].code === code) return true;
  }
  return false;
}

// obConnLine 连接目标一行：连的哪台机、哪个端口、端口哪来的。
// 「端口来源」必须显示——"连上了"不等于"连对了"
function obConnLine(conn) {
  if (!conn) return '';
  if (conn.valid === false) {
    return '<div class="ob-conn">凭据状态：' + obEscape(conn.reason || '不可用') + '</div>';
  }
  var h = '<div class="ob-conn">连接目标 <code>' + obEscape(conn.target || '') + '</code>';
  if (conn.port_source_note) {
    var warn = conn.port_source === 'default_22';
    h += ' · 端口来源：' + (warn ? '<span style="color:#92400e">⚠ ' + obEscape(conn.port_source_note) + '</span>' : obEscape(conn.port_source_note));
  }
  return h + '</div>';
}

// ---------------- 步骤结论条 + 主机资源信息卡（2026-09-22 用户拍板） ----------------
// 结论条：没问题一句"通过"带关键信息；有问题给原因与下一步动作——
// 运维聚焦的是"能不能装、哪里坏了、我该干什么"，不是一排技术细节
// obDisplaySummary 展示层摘要（2026-09-22）：等人工确认的决策卡环节，老事件里
// 可能残留改版前的文案（如零插件时的"会连带停掉它们"）——统一替换成中性说明，
// 影响范围以决策卡为准。meta 行与结论条共用，避免两处口径不一
function obDisplaySummary(e) {
  var txt = e.summary || '';
  if ((e.status || 'pending') === 'blocked') {
    var dmx = obDetail(e);
    if (e.step === 'confirm_uninstall' && dmx && dmx.waiting_for === 'human_confirm_offboard') return '待人工确认——影响范围以下方决策卡为准';
    if (e.step === 'confirm_upgrade' && dmx && dmx.waiting_for === 'human_confirm_upgrade') return '待人工确认——升级方案以下方决策卡为准';
    // 启停闸门已移除（2026-09-24 用户评审："停用，需要我确认？确认什么东西？？？"）。
    // 旧流水线快照里还留着"待人工确认"，如实标注为旧版闸门，免得人以为还有个动作要点头
    if (e.step === 'confirm_service' && dmx && dmx.waiting_for === 'human_confirm_service') return '旧版启停闸门（已移除）——启停现在核对通过即执行，无需人工确认';
  }
  return txt;
}

// obLegacyServiceGate 历史事件归一（2026-09-24）：启停的"确认"环节已改为只做核对并自动放行，
// 但闸门移除前落库的快照里还带着旧标题与旧结论。它们是当时模板的产物，如实改成现在的语义
function obLegacyServiceGate(e) {
  var dmx = obDetail(e);
  if (!dmx || dmx.waiting_for !== 'human_confirm_service') return e;
  if (e.step === 'confirm_service') return Object.assign({}, e, { title: '核对启停影响' });
  if (e.step === 'service_execute' && (e.status || '') === 'blocked') {
    return Object.assign({}, e, { summary: '旧版启停闸门（已移除）——当时被卡住未执行' });
  }
  return e;
}

function obConclusion(st, e, dd) {
  if (st === 'running' || st === 'pending') return '';
  var title = obEscape(e.title || e.step);
  if (st === 'ok') {
    var chips = obConclusionChips(dd);
    var h = '<div class="ob-concl ok">✓ <b>' + title + ' · 通过</b>';
    if (chips) h += '<span class="chips">' + chips + '</span>';
    h += '</div>';
    return h;
  }
  // skipped：不适用当前模式/配置而未执行——不是失败，更不能给"点重试"的建议。
  // （曾把"跳过"渲染成「✕ 未通过」并附重试指引，让运维误以为出故障；2026-09-22 用户现场指出）
  if (st === 'skipped') {
    var h4 = '<div class="ob-concl skip">– <b>' + title + ' · 已跳过</b>';
    var whySkip = obDisplaySummary(e) || '本步不适用当前模式或配置，平台未执行';
    h4 += '<div class="why">' + obEscape(whySkip) + '</div>';
    h4 += '</div>';
    return h4;
  }
  var bad = obLeadingDiag(dd);
  if (st === 'blocked') {
    var h2 = '<div class="ob-concl wait">⏸ <b>' + title + ' · 待人工决策</b>';
    var whyTxt = obDisplaySummary(e);
    if (whyTxt) h2 += '<div class="why">' + obEscape(whyTxt) + '</div>';
    h2 += '</div>';
    return h2;
  }
  // fail：问题原因 + 下一步动作（有诊断建议用诊断建议，没有给通用指引）
  var h3 = '<div class="ob-concl fail">✕ <b>' + title + ' · 未通过</b>';
  // 诊断 code 随结论条走：下方诊断框会把这条略过，code 是排障检索的抓手，不能丢
  if (bad && bad.code) h3 += '<span class="code">' + obEscape(bad.code) + '</span>';
  var why = (bad && bad.message) || e.summary || '';
  if (why) h3 += '<div class="why">' + obEscape(why) + '</div>';
  var next = (bad && bad.hint) || '点下方「↻ 重试该步」重试一次；仍失败时展开「原始输出」定位原因';
  h3 += '<div class="next">下一步：' + obEscape(next) + '</div>';
  h3 += '</div>';
  return h3;
}

// obLeadingDiag 结论条引用哪条诊断：fatal 优先，其次第一条非 info。
// 结论条与诊断框共用同一判据，才能保证「结论条说过的，诊断框不再重复」
function obLeadingDiag(dd) {
  var dgs = (dd && dd.diagnosis) || [];
  var bad = null;
  for (var i = 0; i < dgs.length; i++) {
    var d = dgs[i] || {};
    if (d.level === 'fatal') return d;
    if (d.level !== 'info' && !bad) bad = d;
  }
  return bad;
}

// obConclusionChips 通过结论里的关键信息串（探路步带 facts 才有）
function obConclusionChips(dd) {
  var fc = dd && dd.facts;
  if (!fc) return '';
  var parts = [];
  if (fc.distribution) parts.push(obEscape(fc.distribution));
  if (fc.arch) parts.push(obEscape(fc.arch === 'amd64' ? '64位' : fc.arch));
  if (fc.mem_mb > 0) parts.push('内存 ' + (fc.mem_mb / 1024).toFixed(0) + 'GB');
  if (fc.disk_free_gb > 0) parts.push('磁盘余 ' + fc.disk_free_gb.toFixed(0) + 'GB');
  if (fc.probe_port) parts.push('端口 ' + fc.probe_port + ' ✓');
  if (fc.console_reach && fc.console_reach !== 'FAIL') parts.push('回连 ✓');
  return parts.join(' · ');
}

// obFactsCard 主机资源信息卡：探路采到的机器档案结构化展示，默认收起。
// 数据同源落库（resources.probe_json），未来资源信息反哺直接取台账
function obFactsCard(fc, idx) {
  if (!fc || (!fc.distribution && !fc.hostname && !fc.os)) return '';
  var gb = function (mb) { return (mb / 1024).toFixed(0) + ' GB'; };
  var na = '<span style="color:var(--muted)">未采到</span>';
  var rows = [
    ['操作系统', fc.distribution ? obEscape(fc.distribution) : (fc.os ? obEscape(fc.os) : na)],
    ['架构', fc.arch ? obEscape(fc.arch === 'amd64' ? 'x86_64（64位）' : fc.arch) : na],
    ['内核', fc.kernel ? obEscape(fc.kernel) : na],
    ['主机名', fc.hostname ? obEscape(fc.hostname) + (fc.fqdn && fc.fqdn !== fc.hostname ? '<span style="color:var(--muted)">（FQDN: ' + obEscape(fc.fqdn) + '）</span>' : '') : na],
    ['CPU', fc.cpu_vcpus > 0 ? fc.cpu_vcpus + ' 核' + (fc.cpu_model ? ' · ' + obEscape(fc.cpu_model) : '') : na],
    ['内存', fc.mem_mb > 0 ? gb(fc.mem_mb) : '<span style="color:#92400e">0（未采到，不代表真实值）</span>'],
    ['磁盘可用', fc.disk_free_gb > 0 ? fc.disk_free_gb.toFixed(1) + ' GB（根分区）' : '<span style="color:#92400e">0（未采到，不代表真实值）</span>'],
    ['IP 地址', fc.ipv4_default ? obEscape(fc.ipv4_default) + (fc.ipv4_all && fc.ipv4_all.length > 1 ? '<span style="color:var(--muted)">（共 ' + fc.ipv4_all.length + ' 个 IPv4，含容器/网桥）</span>' : '') : (fc.ipv4_all && fc.ipv4_all.length ? obEscape(fc.ipv4_all.join(', ')) : na)],
    ['虚拟化', fc.virt ? obEscape(fc.virt) : na],
    ['硬件', fc.product ? obEscape(fc.product) : na],
    ['Python', fc.python_version ? obEscape(fc.python_version) : na],
    ['运行时长', fc.uptime_sec > 0 ? obDur(fc.uptime_sec * 1000) : na],
    ['探测方式', obEscape((fc.probed_by || '') + (fc.probed_at ? ' · ' + fc.probed_at : '') + (fc.probe_port ? ' · SSH 端口 ' + fc.probe_port : ''))]
  ];
  var id = 'ob-facts-' + (idx == null ? 'x' : idx);
  var h = '<div class="ob-facts">';
  h += '<div class="hd2"><button class="btn btn-o btn-sm" onclick="obToggleAny(\'' + id + '\')">🖥 主机资源信息（已存入资源台账，可反哺资产档案）</button></div>';
  h += '<div id="' + id + '" style="display:none"><table>';
  for (var i = 0; i < rows.length; i++) h += '<tr><td>' + rows[i][0] + '</td><td>' + rows[i][1] + '</td></tr>';
  h += '</table></div></div>';
  return h;
}

// obEvidenceText 把 evidence 里的原始输出拼成人可读文本（带分段标题）。
// 没有 evidence 时返回空串，前端回落到完整 detail JSON——旧事件的详情依然可看
function obEvidenceText(dd) {
  if (!dd || !dd.evidence || typeof dd.evidence !== 'object') return '';
  var ev = dd.evidence;
  var order = [
    ['failed_task_lines', '★ 失败任务原文（首个 FAILED! 就是真因）'],
    ['ping_output', '① 联通性测试 ansible ping'],
    ['sshd_config_output', '② 目标机 sshd_config 的 Port 声明'],
    ['listen_output', '③ 目标机本机监听端口（ss/netstat）'],
    ['setup_output', '④ 事实采集 ansible setup --tree'],
    ['df_output', '⑤ 磁盘兜底采集 df -k /'],
    ['output', '⑥ 安装 ansible-playbook 全量输出'],
    ['syntax_output', '⑦ playbook 语法校验输出'],
    ['port_check', '⑦ 端口核对结构化结果']
  ];
  var parts = [], seen = {};
  function body(v) { return typeof v === 'string' ? v : JSON.stringify(v, null, 2); }
  for (var i = 0; i < order.length; i++) {
    var k = order[i][0];
    seen[k] = true;
    if (ev[k] === undefined || ev[k] === null || ev[k] === '') continue;
    parts.push('===== ' + order[i][1] + ' =====\n' + body(ev[k]));
  }
  // 后续新增的证据字段也要带上，避免"后端加了证据、界面看不见"
  for (var kk in ev) {
    if (seen[kk] || ev[kk] === undefined || ev[kk] === null || ev[kk] === '') continue;
    parts.push('===== ' + kk + ' =====\n' + body(ev[kk]));
  }
  return parts.join('\n\n');
}

// obExecutorBox 执行包：可复制命令 + 探路回报结构化表单（替代自由文本 prompt）
function obExecutorBox(f, d, i) {
  var h = '';
  h += '<div style="margin:8px 0;padding:8px 10px;border:1px solid var(--border);border-radius:6px;background:var(--bg)">';
  h += '<div style="font-size:11px;color:var(--muted);margin-bottom:4px">📋 执行包：复制到可连通目标机的机器上执行，把输出填入下方表单</div>';
  h += '<pre id="ob-ins-' + i + '" style="margin:0 0 6px;padding:8px;background:var(--bg2);border-radius:4px;font-size:11px;white-space:pre-wrap;user-select:all">' + obEscape(d.instruction) + '</pre>';
  h += '<button class="btn btn-o btn-sm" onclick="obCopyText(\'ob-ins-' + i + '\')">复制命令</button>';
  h += '</div>';
  h += '<div style="margin:8px 0;padding:8px 10px;border:1px solid var(--border);border-radius:6px">';
  h += '<div style="font-size:11px;color:var(--muted);margin-bottom:6px">探路回报（带 * 必填，平台校验后回填资源台账）</div>';
  h += '<div style="display:flex;flex-wrap:wrap;gap:8px;align-items:center;font-size:12px">';
  h += '<label>OS* <select id="ob-p-os" style="padding:2px 4px"><option value=""></option><option>linux</option><option>windows</option><option>darwin</option></select></label>';
  h += '<label>架构* <select id="ob-p-arch" style="padding:2px 4px"><option value=""></option><option>x86_64</option><option>aarch64</option><option>armv7l</option></select></label>';
  h += '<label>内核 <input id="ob-p-kernel" size="14" placeholder="uname -r 输出"></label>';
  h += '<label>内存MB <input id="ob-p-mem" size="7" placeholder="free 输出"></label>';
  h += '<label>磁盘GB <input id="ob-p-disk" size="7" placeholder="df 可用"></label>';
  h += '</div>';
  h += '<div style="margin-top:8px"><button class="btn btn-d btn-sm" onclick="obSubmitProbe(' + f.id + ')">提交探路结果 → 进入版本选择</button></div>';
  h += '</div>';
  return h;
}

function obVal(id) { var el = document.getElementById(id); return el ? el.value.trim() : ''; }

function obSubmitProbe(fid) {
  var os = obVal('ob-p-os'), arch = obVal('ob-p-arch');
  if (!os || !arch) { alert('OS 与架构为必填——这正是用来决定装哪个版本的信息'); return; }
  fetch('/api/onboard/flow/probe', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      flow_id: fid, os: os, arch: arch,
      kernel: obVal('ob-p-kernel'),
      mem_mb: parseFloat(obVal('ob-p-mem')) || 0,
      disk_free_gb: parseFloat(obVal('ob-p-disk')) || 0
    })
  }).then(function (r) { return r.json(); }).then(function (d) {
    if (d.ok) { obRefreshFlow(fid); }
    else { alert('探路回填被拒：' + (d.error || '未知原因')); }
  }).catch(function (e) { alert('请求失败：' + e); });
}

// obVersionPicker 版本点选：兼容清单由平台按实测 os/arch 过滤（stable 靠前），
// 默认选中第一个 stable；deprecated 需人工显式点选（无默认）
function obVersionPicker(f, d) {
  var h = '';
  h += '<div style="margin:8px 0;padding:8px 10px;border:1px solid var(--border);border-radius:6px">';
  h += '<div style="font-size:11px;color:var(--muted);margin-bottom:6px">版本清单（实测 ' + obEscape(d.os || '') + '/' + obEscape(d.arch || '') + ' 兼容项）——由人工选定，选择将留痕审计。版本号即 SAgent 版本；tag 是发布包标识（系统-架构-版本）：</div>';
  var firstStable = true;
  for (var k = 0; k < d.candidates.length; k++) {
    var v = d.candidates[k];
    var dep = v.status !== 'stable';
    var checked = dep ? '' : (firstStable ? ' checked' : '');
    if (!dep) firstStable = false;
    h += '<label style="display:flex;gap:8px;align-items:center;padding:4px 0;font-size:12px;cursor:pointer">';
    h += '<input type="radio" name="ob-ver" value="' + obEscape(v.tag) + '"' + checked + '>';
    h += '<b>SAgent ' + obEscape(v.version || '') + '</b>';
    h += '<span style="color:var(--muted)">' + obEscape(v.tag) + '</span>';
    h += '<span class="badge ' + (dep ? 'b-o' : 'b-r') + '" style="font-weight:400">' + obEscape(v.status === 'stable' ? '正式版' : v.status === 'deprecated' ? '旧版' : v.status) + '</span>';
    h += '<span style="color:var(--muted)">' + obEscape(v.released || '') + (v.notes ? ' · ' + obEscape(v.notes) : '') + '</span>';
    h += '</label>';
  }
  h += '<div style="margin-top:8px"><button class="btn btn-d btn-sm" onclick="obPickVersion(' + f.id + ')">确认选择并继续流程</button></div>';
  h += '</div>';
  return h;
}

function obPickVersion(fid) {
  var el = document.querySelector('input[name="ob-ver"]:checked');
  if (!el) { alert('请先选择一个版本'); return; }
  if (!confirm('确认选定 ' + el.value + ' 并继续流程？选版会立即触发后续安装环节，操作将留痕审计。')) { return; }
  fetch('/api/onboard/flow/version', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ flow_id: fid, tag: el.value })
  }).then(function (r) { return r.json(); }).then(function (d) {
    if (d.ok) { obRefreshFlow(fid); }
    else { alert('版本确认被拒：' + (d.error || '未知原因')); }
  }).catch(function (e) { alert('请求失败：' + e); });
}

// 需要渲染「结论面板」的卸载前置步骤：扫描 / 卸插件 / 清自启。
// 用表驱动而不是散落的 if：加一个步骤只加一行，不会漏
var OB_OFFBOARD_PANELS = { scan_collectors: 1, uninstall_plugins: 1, cleanup_autostart: 1 };

function obFormText(f) {
  return { builtin: '进程内建', subprocess: '独立子进程', exec: '脚本', unknown: '未知' }[f] || (f || '未知');
}
function obProcText(p) {
  return { yes: '有', no: '无', unknown: '无法判定' }[p] || (p || '未知');
}
function obAutoText(k) {
  return { run_sh: '自愈守护 run.sh', cron_user: '用户 crontab', cron_system: '系统级 crontab' }[k] || k;
}
function obAutoStateText(s) {
  return { running: '运行中', present: '存在', absent: '无', unknown: '未能核实' }[s] || s;
}

// obOffboardTables 扫描清单表格：插件表 + 自愈表（确认面板与扫描结论面板共用）
function obOffboardTables(d) {
  var h = '';
  var ps = d.plugins || [];
  if (ps.length) {
    h += '<div style="font-size:11px;color:var(--muted);margin:6px 0 3px">采集插件 ' + ps.length + ' 个</div>';
    h += '<table style="width:100%;font-size:11px;border-collapse:collapse">';
    h += '<tr style="color:var(--muted);text-align:left"><th style="padding:2px 6px 2px 0;font-weight:400">插件</th><th style="font-weight:400">形态</th><th style="font-weight:400">来源</th><th style="font-weight:400">独立进程</th><th style="font-weight:400">产物</th></tr>';
    for (var i = 0; i < ps.length; i++) {
      var p = ps[i];
      var src = p.source === 'config' ? (p.enabled ? '配置启用' : '配置未启用') : '目录实存·未启用';
      var warn = (p.process === 'yes' || p.process === 'unknown') ? 'color:#b45309' : '';
      h += '<tr style="border-top:1px solid var(--border)">';
      h += '<td style="padding:3px 6px 3px 0"><b>' + obEscape(p.name) + '</b></td>';
      h += '<td>' + obEscape(obFormText(p.form)) + '</td>';
      h += '<td>' + obEscape(src) + '</td>';
      h += '<td style="' + warn + '">' + obEscape(obProcText(p.process)) + '</td>';
      h += '<td style="color:var(--muted)">' + obEscape(p.path || '-') + '</td>';
      h += '</tr>';
    }
    h += '</table>';
  } else {
    h += '<div style="font-size:11px;color:var(--muted);margin:6px 0 3px">采集插件：未发现</div>';
  }
  var as = d.autostart || [];
  if (as.length) {
    h += '<div style="font-size:11px;color:var(--muted);margin:8px 0 3px">自愈与自启来源</div>';
    for (var k = 0; k < as.length; k++) {
      var a = as[k];
      var hit = (a.state === 'running' || a.state === 'present');
      h += '<div style="font-size:11px;padding:2px 0;' + (hit ? 'color:#b45309' : 'color:var(--muted)') + '">· ';
      h += obEscape(obAutoText(a.kind)) + '：' + obEscape(obAutoStateText(a.state));
      // 只有 crontab 的 hits 才是"命中几条"；run.sh 的 hits 恒为 1（就一个守护位），
      // 显示成「无（1 条）」会自相矛盾 —— 状态已经说清了，别再加一个数
      if (a.hits && (a.kind === 'cron_user' || a.kind === 'cron_system')) h += '（' + a.hits + ' 条）';
      if (a.pid) h += ' pid=' + obEscape(a.pid);
      h += '</div>';
    }
  }
  return h;
}

// obOffboardConfirm 卸载前的人工确认决策卡（2026-09-22 重设计）。
// 设计目标：操作者只回答一个问题——"同意卸载吗"。三行定性（将删除/不影响或将停止/待复核）
// 就是全部决策输入；扫描结论表、停止/删除/保留全文沉进「完整影响清单」折叠屉——
// 信息一条不少，但不再铺满一屏。will_stop 的兜底文案（无插件时）单独识别，
// 渲染成绿色的「不影响」而不是吓人的"会停掉"
function obOffboardConfirm(f, d) {
  var stop = d.will_stop || [], rem = d.will_remove || [], keep = d.will_keep || [];
  var FALLBACK = '目标机上未发现启用的采集插件与自愈来源：卸载只影响 SAgent 本体';
  var realStop = [];
  for (var i = 0; i < stop.length; i++) if (stop[i] !== FALLBACK) realStop.push(stop[i]);
  var h = '';
  h += '<div style="margin:8px 0;border:2px solid #087d75;border-radius:8px;padding:12px 14px;background:var(--bg)">';
  h += '<div style="font-size:13px;font-weight:600">确认卸载影响范围</div>';
  h += '<div style="font-size:11px;color:var(--muted);margin:2px 0 10px">以下内容来自目标机实测扫描 · ' + obEscape(d.scan_at || '未知时间') + ' · 确认之前不会对目标机做任何改动</div>';
  h += '<div style="display:grid;grid-template-columns:64px 1fr;gap:4px 12px;font-size:12px;line-height:1.7">';
  h += '<div style="color:#b91c1c">将删除</div><div>';
  for (var j = 0; j < rem.length; j++) h += '<div>· ' + obEscape(rem[j]) + '</div>';
  h += '</div>';
  if (realStop.length) {
    h += '<div style="color:#b45309">将停止</div><div>';
    for (var s2 = 0; s2 < realStop.length; s2++) h += '<div>· ' + obEscape(realStop[s2]) + '</div>';
    h += '</div>';
  } else {
    h += '<div style="color:#047857">不影响</div><div>' + obEscape(FALLBACK) + '</div>';
  }
  if (d.cron_unverified) {
    h += '<div style="color:#b45309">待复核</div><div>用户 crontab 未能读取核实，可能仍有指向 SAgent 的定时任务——建议卸载后上机 <code>crontab -l</code> 复核</div>';
  }
  h += '</div>';
  // 完整影响清单折叠屉：明细一条不删，只是默认不展开
  var detId = 'ob-obd-' + f.id;
  h += '<div style="margin-top:8px;font-size:12px"><span id="' + detId + '-lk" style="color:#087d75;cursor:pointer" onclick="obObdToggle(\'' + detId + '\')">▸ 完整影响清单（扫描结论与明细）</span></div>';
  h += '<div id="' + detId + '" style="display:none;margin-top:6px;background:var(--bg);border:0.5px solid var(--border);border-radius:6px;padding:8px 10px">';
  h += obOffboardTables(d);
  if (stop.length) {
    h += '<div style="font-size:11px;margin-top:8px;color:#92400e"><b>点「确认卸载」之后会停掉：</b></div>';
    for (var i2 = 0; i2 < stop.length; i2++) h += '<div style="font-size:11px;padding:2px 0 0 10px">· ' + obEscape(stop[i2]) + '</div>';
  }
  if (rem.length) {
    h += '<div style="font-size:11px;margin-top:6px;color:var(--muted)"><b>会被删除：</b></div>';
    for (var j2 = 0; j2 < rem.length; j2++) h += '<div style="font-size:11px;padding:2px 0 0 10px;color:var(--muted)">· ' + obEscape(rem[j2]) + '</div>';
  }
  if (keep.length) {
    h += '<div style="font-size:11px;margin-top:6px;color:var(--muted)"><b>会保留：</b></div>';
    for (var m2 = 0; m2 < keep.length; m2++) h += '<div style="font-size:11px;padding:2px 0 0 10px;color:var(--muted)">· ' + obEscape(keep[m2]) + '</div>';
  }
  h += '</div>';
  h += '<div style="margin-top:10px;display:flex;gap:8px;align-items:center;flex-wrap:wrap">';
  h += '<button class="btn btn-d btn-sm" onclick="obOffboardDecision(' + f.id + ',\'proceed\')">确认卸载并继续</button>';
  h += '<button class="btn btn-o btn-sm" onclick="obOffboardDecision(' + f.id + ',\'cancel\')">取消卸载</button>';
  h += '<span style="font-size:11px;color:var(--muted)">操作与理由写入审计日志</span>';
  h += '</div>';
  h += '</div>';
  return h;
}

// obObdToggle 决策卡折叠屉开关（箭头随状态翻转）
function obObdToggle(id) {
  var bd = document.getElementById(id), lk = document.getElementById(id + '-lk');
  if (!bd) return;
  var open = bd.style.display !== 'none';
  bd.style.display = open ? 'none' : 'block';
  if (lk) lk.textContent = open ? '▸ 完整影响清单（扫描结论与明细）' : '▾ 收起完整影响清单';
}

// obOpsDecision 升级决策卡的确认/取消提交（与卸载确认同一通道语义）
function obOpsDecision(flowId, mode, decision) {
  var reason = '';
  if (decision === 'cancel') {
    reason = prompt('取消升级的原因（留痕，可直接确定跳过）：') || '';
    if (reason === null) return;
  }
  fetch(API + '/onboard/flow/' + mode + '/confirm', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ flow_id: flowId, decision: decision, reason: reason })
  }).then(function (r) { return r.json(); }).then(function (o) {
    if (o && o.error) { toast('操作被拒：' + o.error, 'err'); return; }
    toast(decision === 'proceed' ? '已确认，流水线继续执行' : '已取消，目标机未做任何改动', 'ok');
    setTimeout(function () { openOnboardFlow(flowId); }, 400);
  }).catch(function (e) { toast('操作失败：' + (e && e.message || e), 'err'); });
}

// obUpgradeConfirm 升级决策卡：当前版本 → 目标版本（降级显眼提示）+ 三行定性 + 两个按钮
function obUpgradeConfirm(f, d) {
  var h = '<div style="margin:8px 0;padding:10px 12px;border:1px solid ' + (d.downgrade ? '#e24b4a' : '#f59e0b') + ';border-radius:6px;background:' + (d.downgrade ? '#fcebeb' : '#fffbeb') + '">';
  if (d.downgrade) {
    h += '<div style="font-size:12px;font-weight:500;color:#791f1f">⚠ 降级操作：目标版本低于当前版本。降级前请确认新版存在不兼容问题，决策将留痕</div>';
  } else {
    h += '<div style="font-size:12px;font-weight:500;color:#92400e">⚠ 升级前确认：以下内容来自目标机实测（升级前预检）</div>';
  }
  h += '<div style="background:var(--bg);border-radius:4px;padding:8px 10px;margin-top:8px;font-size:12px">';
  h += '<div style="display:flex;gap:8px;padding:2px 0"><span style="color:var(--muted);width:76px">当前版本</span><b>' + obEscape(d.current_version || '未知（版本清单外）') + '</b><span style="color:var(--muted);font-size:11px">' + obEscape((d.current_sha || '').slice(0, 12)) + '</span></div>';
  h += '<div style="display:flex;gap:8px;padding:2px 0"><span style="color:var(--muted);width:76px">目标版本</span><b>' + obEscape(d.target_version + '（' + d.target_tag + '）') + '</b></div>';
  h += '<div style="display:flex;gap:8px;padding:2px 0"><span style="color:var(--muted);width:76px">守护形态</span><span>' + (d.guard ? 'run.sh 守护在跑（按守护协议启停）' : '无守护（直杀/直启）') + '</span></div>';
  h += '<div style="display:flex;gap:8px;padding:2px 0"><span style="color:var(--muted);width:76px">磁盘可用</span><span>' + (d.disk_free_mb || 0) + ' MB</span></div>';
  h += '</div>';
  h += '<div style="font-size:11px;margin-top:8px;color:#92400e"><b>升级期间采集中断约数十秒。</b>将执行：停插件 → 停进程 → 备份旧二进制 → 换包（sha256 断言）→ 启动 → 自证核对</div>';
  h += '<div style="font-size:11px;margin-top:4px;color:var(--muted)">不受影响：配置（conf）、数据（data/logs）、插件（plugins）、平台登记与采集目标。旧二进制备份为 ' + obEscape('SAgent.bak-' + (d.current_tag || 'backup')) + ' 留在目标机</div>';
  if (d.same_version) h += '<div style="font-size:11px;margin-top:4px;color:#92400e">目标版本与当前版本相同——本次为同版本重装，请确认这是你的本意</div>';
  h += '<div style="margin-top:10px;display:flex;gap:8px">';
  h += '<button class="btn btn-d btn-sm" onclick="obOpsDecision(' + f.id + ',\'upgrade\',\'proceed\')">确认升级并继续</button>';
  h += '<button class="btn btn-o btn-sm" onclick="obOpsDecision(' + f.id + ',\'upgrade\',\'cancel\')">取消升级</button>';
  h += '<span style="font-size:11px;color:var(--muted);align-self:center">操作者 admin · 决策留痕</span>';
  h += '</div></div>';
  return h;
}

// obRowToggle 卸载版面折叠行开关（「▼ 明细 / ▲ 收起」）
function obRowToggle(id) {  var bd = document.getElementById(id), ar = document.getElementById(id + '-ar');
  if (!bd) return;
  var open = bd.style.display !== 'none';
  bd.style.display = open ? 'none' : 'block';
  if (ar) ar.textContent = open ? '▼ 明细' : '▲ 收起';
}

// obOffboardOutcome 卸载前置步骤的结论面板（扫描 / 卸插件 / 清自启）
function obOffboardOutcome(step, st, d) {
  if (!d) return '';
  var h = '<div style="margin:8px 0;padding:8px 10px;border:1px solid var(--border);border-radius:6px;background:var(--bg)">';
  if (step === 'scan_collectors') {
    var sc = d.scan || d;
    h += '<div style="font-size:11px;color:var(--muted)">只读扫描结论（未改动目标机）· ' + obEscape(sc.scanned_at || '') + '</div>';
    h += obOffboardTables(sc);
  } else if (step === 'uninstall_plugins') {
    var v = d.plugins_verdict || '未知';
    var bad = (st === 'fail' || v === 'dirty');
    h += '<div style="font-size:11px;color:' + (bad ? '#b91c1c' : '#047857') + '">';
    h += bad ? '✗ 仍有插件进程未停止（目标机核对未通过）' : '✓ 插件进程已全部停止（目标机自证核对通过）';
    h += '</div>';
    if (d.verify_line) h += '<div style="font-size:11px;color:var(--muted);margin-top:3px">核对：<code style="font-size:11px">' + obEscape(d.verify_line) + '</code></div>';
    h += '<div style="font-size:11px;color:var(--muted);margin-top:3px">插件产物（bin/vector、bin/mysqld_exporter 等）随下一步「卸载 SAgent」清理安装目录时一并删除。</div>';
  } else if (step === 'cleanup_autostart') {
    var v2 = d.autostart_verdict || '未知';
    var partial = (v2 === 'partial');
    var bad2 = (st === 'fail' || v2 === 'dirty');
    h += '<div style="font-size:11px;color:' + (bad2 ? '#b91c1c' : (partial ? '#b45309' : '#047857')) + '">';
    h += bad2 ? '✗ 自愈守护或 crontab 仍有残留（核对未通过）'
              : (partial ? '⚠ 自愈守护已清理；crontab 未能核实，需人工复核' : '✓ 自愈守护与 crontab 均已清理（目标机自证核对通过）');
    h += '</div>';
    if (d.verify_line) h += '<div style="font-size:11px;color:var(--muted);margin-top:3px">核对：<code style="font-size:11px">' + obEscape(d.verify_line) + '</code></div>';
  }
  h += '</div>';
  return h;
}

// obOffboardDecision 提交卸载确认决策（proceed / cancel）
function obOffboardDecision(fid, decision) {
  var reason = '';
  if (decision === 'proceed') {
    if (!confirm('确认卸载？\n\n接下来会依次：停止 SAgent 托管的采集插件 → 清理自愈守护与 crontab → 卸载 SAgent 本身 → 注销平台登记。\n\n资源对象与 SSH 凭据会保留，之后可原样重新接入。\n\n本操作将留痕审计。')) return;
  } else {
    reason = prompt('取消卸载的原因（会留痕审计）：', '') || '';
    if (reason === null) return;
    if (!confirm('确认取消本次卸载？目标机与平台登记都不会有任何改动。')) return;
  }
  fetch('/api/onboard/flow/offboard/confirm', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ flow_id: fid, decision: decision, reason: reason })
  }).then(function (r) { return r.json(); }).then(function (d) {
    if (d.ok) { obRefreshFlow(fid); }
    else { alert('操作被拒：' + (d.error || '未知原因')); }
  }).catch(function (e) { alert('请求失败：' + e); });
}

function obCopyText(id) {
  var el = document.getElementById(id);
  if (!el) return;
  var t = el.textContent;
  if (navigator.clipboard && navigator.clipboard.writeText) { navigator.clipboard.writeText(t); return; }
  var ta = document.createElement('textarea');
  ta.value = t; document.body.appendChild(ta); ta.select();
  document.execCommand('copy'); document.body.removeChild(ta);
}

function obDur(ms) {
  if (ms < 1000) return ms + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  return Math.floor(ms / 60000) + 'm' + Math.round((ms % 60000) / 1000) + 's';
}

function obDefaultSummary(st, e) {
  if (st === 'pending') return '等待前序环节完成';
  if (st === 'running') return '执行中…';
  if (st === 'ok') return '已完成';
  if (st === 'skipped') return '已跳过（不适用当前模式）';
  if (st === 'blocked') return '被阻断：前置门禁未通过，不生成配置、不下发';
  if (st === 'fail') return '执行失败';
  return '';
}

function obPretty(detail) {
  if (typeof detail !== 'string') return JSON.stringify(detail, null, 2);
  var s = detail.trim();
  if (s.charAt(0) === '{' || s.charAt(0) === '[') {
    try { return JSON.stringify(JSON.parse(s), null, 2); } catch (e) { /* 非 JSON，原样 */ }
  }
  return detail;
}

function obToggleDet(i) {
  var el = document.getElementById('ob-det-' + i);
  if (el) el.style.display = el.style.display === 'none' ? 'block' : 'none';
}

// obToggleAny 通用折叠开关（技术细节 / 资源信息卡等）
function obToggleAny(id) {
  var el = document.getElementById(id);
  if (el) el.style.display = el.style.display === 'none' ? 'block' : 'none';
}

function obRefreshFlow(id) { openOnboardFlow(id); }

// ===================================================================
//  流水线详情自动刷新（2026-09-24 用户评审：「卸载界面需要支持自刷新数据（不刷界面），
//  不然扫描范围这个环节跳不到，一直在选择对象界面；后续的动作也一样，3-5 秒一刷」）
//
//  为什么要轮询：卸载的推进靠目标机侧 ansible 异步回报（选择对象 → 扫描 → 确认 → 卸载…），
//  每一步都由后端在别处推进，前端只观测。没有轮询时，人看到的就是停在「选择对象」不动，
//  必须手点「↻ 刷新」才知道已经跑到哪了。
//
//  「不刷界面」= 不重建整页、不关弹层：每次只把内容真的变了的区块换掉，
//  没变的区块 DOM 一个字节都不动——折叠屉的展开状态、日志的滚动位置、
//  正在看的那一行都不会被顶掉（整页重渲染会把人正在看的东西冲走）。
// ===================================================================

var OB_FLOW_POLL_MS = 4000;   // 3-5 秒区间，取中值：够快看得见推进，也不至于把后端打热
var _obFlowPollTimer = null;
var _obFlowPollId = null;
var _obFlowPollSec = {};

// 步骤式操作台各分区的渲染顺序（键名即 obFlowSections 里的分区名，也就是 DOM 上的
// ob-flow-sec-<name> 后缀）。卸载 / 接入 / 启停三套向导共用这一份顺序，
// 自动刷新按此逐区比对，顺序必须与 obFlowBody 一致
var OB_WZ_SEC_ORDER = ['obj', 'steps', 'concl', 'gate', 'dur', 'term', 'check', 'det'];

// obFlowStatusLive 还在推进吗：done/failed/cancelled 之外的都要盯着
// （blocked = 等人工确认，人可能在别处点了确认，所以也要盯）
function obFlowStatusLive(st) {
  return st !== 'done' && st !== 'failed' && st !== 'cancelled' && st !== 'canceled';
}

function obFlowPollStop() {
  if (_obFlowPollTimer) { clearInterval(_obFlowPollTimer); _obFlowPollTimer = null; }
  _obFlowPollId = null;
  _obFlowPollSec = {};
}

// obFlowPollStart 开始盯这条流水线。先把当前各分区内容记为基线，
// 这样第一次轮询若数据没变，就不会做任何 DOM 操作
function obFlowPollStart(id, d) {
  obFlowPollStop();
  if (!d || !d.flow || !obFlowStatusLive(d.flow.status)) return;
  _obFlowPollId = id;
  var S = obFlowSections(d);
  for (var k in S) _obFlowPollSec[k] = S[k];
  _obFlowPollTimer = setInterval(function () { obFlowPollTick(id); }, OB_FLOW_POLL_MS);
}

// obFlowPollTick 单次轮询：弹层已被关掉/换页就自行收工，不打扰别的页面
function obFlowPollTick(id) {
  if (_obFlowPollId !== id) return;
  var body = document.getElementById('overlay-body');
  // data-ob-flow 是打开时打上的印记——弹层还在但不是这条流水线（人换了别的页面），
  // 就停掉，否则会把流水线内容糊到别人的弹层上
  if (!body || body.getAttribute('data-ob-flow') !== String(id)) { obFlowPollStop(); return; }
  fetch(API + '/onboard/flow?id=' + id).then(function (r) { return r.json(); }).then(function (d) {
    if (_obFlowPollId !== id) return;
    if (!d || !d.flow) return;
    obFlowPatch(d);
    if (!obFlowStatusLive(d.flow.status)) obFlowPollStop();   // 办结了就别再拉
  }).catch(function () { /* 单次拉取失败不打断轮询，下个周期再试 */ });
}

// obFlowPatch 就地更新：逐分区比对，只替换内容变了的
function obFlowPatch(d) {
  var body = document.getElementById('overlay-body');
  if (!body || body.getAttribute('data-ob-flow') !== String(d.flow.id)) { obFlowPollStop(); return; }
  if (!d.flow.is_offboard && !d.flow.is_onboard && !d.flow.is_service) { body.innerHTML = obFlowBody(d); return; }  // 非步骤式版面没有分区壳，整块重画
  var S = obFlowSections(d);
  var names = ['sum'].concat(OB_WZ_SEC_ORDER, ['act']);
  for (var i = 0; i < names.length; i++) {
    var n = names[i], el = document.getElementById('ob-flow-sec-' + n);
    if (!el) continue;
    var html = S[n];
    if (html == null || _obFlowPollSec[n] === html) continue;   // 内容没变：DOM 一动不动
    _obFlowPollSec[n] = html;
    var keep = obFlowKeepState(el);
    el.innerHTML = html;
    obFlowRestoreState(el, keep);
  }
}

// obFlowKeepState 替换前记下「人手动动过的状态」：展开/收起的折叠块、日志区的滚动位置。
// 自动刷新不该把运维正在看的内容顶掉——本页有多处展开开关（明细折叠屉、二次确认卡的
// 完整影响清单），一律按「有 id 且带内联 display」通用收集，将来再加开关不用改这里
function obFlowKeepState(el) {
  var keep = { disp: {}, text: {}, termScroll: null, termAtBottom: false };
  var nodes = el.querySelectorAll('[id]');
  for (var i = 0; i < nodes.length; i++) {
    var n = nodes[i];
    if (n.style && n.style.display) keep.disp[n.id] = n.style.display;
  }
  // 展开开关的文字（▸/▾、▼/▲）跟着展开态走，不保住就会出现
  // 「清单还开着、链接却写着『展开』」的错位
  var labs = el.querySelectorAll('[id$="-lk"], [id$="-ar"]');
  for (var j = 0; j < labs.length; j++) keep.text[labs[j].id] = labs[j].textContent;
  var bd = el.querySelector('.ob-wz-term .bd');
  if (bd) {
    keep.termScroll = bd.scrollTop;
    // 原本贴着底看最新一行 → 更新后继续贴底；人翻到上面在读 → 保持原位别抢
    keep.termAtBottom = (bd.scrollHeight - bd.scrollTop - bd.clientHeight) < 24;
  }
  return keep;
}

function obFlowRestoreState(el, keep) {
  for (var id in keep.disp) {
    var node = el.querySelector('#' + id);
    if (node) node.style.display = keep.disp[id];
  }
  for (var tid in keep.text) {
    var lab = el.querySelector('#' + tid);
    if (lab) lab.textContent = keep.text[tid];
  }
  var bd = el.querySelector('.ob-wz-term .bd');
  if (bd) bd.scrollTop = keep.termAtBottom ? bd.scrollHeight : keep.termScroll;
}

function obRetry(id, step) {
  fetch(API + '/onboard/flow/step', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ flow_id: id, step: step, action: 'retry' })
  }).then(function (r) { return r.json(); }).then(function () {
    toast('已重试 ' + step, 'ok'); setTimeout(function () { openOnboardFlow(id); }, 400);
  }).catch(function (e) { toast('重试失败：' + (e && e.message || e), 'err'); });
}

function obReport(id, step) {
  var reason = prompt('回报执行结果（带实测证据，将写入审计日志，标记为 executor_report）。\n例如：OS=Linux arch=aarch64 mem=8G disk=828G：');
  if (!reason) return;
  fetch(API + '/onboard/flow/step', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ flow_id: id, step: step, action: 'report', reason: reason })
  }).then(function (r) { return r.json(); }).then(function () {
    toast('已回报，证据已留痕', 'ok'); setTimeout(function () { openOnboardFlow(id); }, 400);
  }).catch(function (e) { toast('回报失败：' + (e && e.message || e), 'err'); });
}

function obForce(id, step) {
  var reason = prompt('强制继续需留痕。请填写理由（将写入审计日志）：');
  if (!reason) return;
  fetch(API + '/onboard/flow/step', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ flow_id: id, step: step, action: 'force', reason: reason })
  }).then(function (r) { return r.json(); }).then(function () {
    toast('已强制继续，理由已留痕', 'ok'); setTimeout(function () { openOnboardFlow(id); }, 400);
  }).catch(function (e) { toast('操作失败：' + (e && e.message || e), 'err'); });
}

// obUpgrade 发起升级流水线（确认框内容来自后端状态，不写死文案）
function obUpgrade(flowId) {
  fetch(API + '/onboard/flow?id=' + flowId).then(function (r) { return r.json(); }).then(function (d) {
    var f = (d && d.flow) || {};
    if (!f.id) { toast('读取流水线失败', 'err'); return; }
    var lines = [];
    lines.push('资源：' + (f.resource_id || '') + (f.resource_ip ? '（' + f.resource_ip + '）' : ''));
    lines.push('');
    lines.push('升级流程：预检当前版本/守护/磁盘（只读）→ 人工选定目标版本（允许降级，留痕）→ 确认影响 → ansible 自动执行（停插件/停进程/备份/换包/sha256 断言/启动/自证核对）');
    lines.push('');
    lines.push('不受影响：配置、数据、插件、平台登记。旧二进制备份留在目标机，可手工回滚');
    lines.push('');
    lines.push('执行方式：' + (f.ssh_ready ? '平台 ansible 自动执行，完成后自动回报' : '⚠ 资源台账缺 SSH 凭据，将回落「下发执行包 + 人工执行回报」'));
    if (!confirm('升级 SAgent\n\n' + lines.join('\n'))) return;
    fetch(API + '/onboard/flow/upgrade', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ flow_id: flowId, resource_id: f.resource_id || '' })
    }).then(function (r) { return r.json(); }).then(function (o) {
      if (o && o.error) { toast('发起升级失败：' + o.error, 'err'); return; }
      toast('已发起升级流水线 flow-' + o.flow_id, 'ok');
      renderOnboardCenter();
      setTimeout(function () { openOnboardFlow(o.flow_id); }, 500);
    }).catch(function (e) { toast('发起升级失败：' + (e && e.message || e), 'err'); });
  }).catch(function (e) { toast('读取流水线失败：' + (e && e.message || e), 'err'); });
}

// obService 发起启停流水线：弹层选动作（停止/启动/重启）与目标（整个 Agent/指定插件）
function obService(flowId) {
  fetch(API + '/onboard/flow?id=' + flowId).then(function (r) { return r.json(); }).then(function (d) {
    var f = (d && d.flow) || {};
    if (!f.id) { toast('读取流水线失败', 'err'); return; }
    var mask = document.createElement('div');
    mask.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,.45);z-index:9998;display:flex;align-items:center;justify-content:center';
    var box = document.createElement('div');
    box.style.cssText = 'background:var(--panel,#fff);border:0.5px solid var(--border);border-radius:12px;padding:16px 18px;width:420px;max-width:92vw;font-size:12px';
    box.innerHTML =
      '<div style="font-weight:500;font-size:13px;margin-bottom:8px">启停 SAgent · ' + obEscape(f.resource_id || '') + (f.resource_ip ? '（' + obEscape(f.resource_ip) + '）' : '') + '</div>' +
      '<div style="margin:8px 0"><div style="color:var(--muted);margin-bottom:4px">动作</div>' +
      '<select id="ob-svc-act" style="width:100%;padding:6px;border:0.5px solid var(--border);border-radius:6px;background:var(--bg)">' +
      '<option value="stop">停止（采集中断，平台标「已停止（维护）」）</option>' +
      '<option value="start">启动（恢复采集）</option>' +
      '<option value="restart">重启（短暂中断，原子完成）</option></select></div>' +
      '<div style="margin:8px 0"><div style="color:var(--muted);margin-bottom:4px">目标</div>' +
      '<select id="ob-svc-tgt" style="width:100%;padding:6px;border:0.5px solid var(--border);border-radius:6px;background:var(--bg)" onchange="document.getElementById(\'ob-svc-pname\').style.display=this.value===\'process\'?\'none\':\'block\'">' +
      '<option value="process">整个 SAgent（进程级）</option>' +
      '<option value="plugin">指定插件（control.sock 单插件操作）</option></select>' +
      '<input id="ob-svc-pname" placeholder="插件名（以预检环节实测的插件清单为准）" style="display:none;width:100%;padding:6px;margin-top:6px;border:0.5px solid var(--border);border-radius:6px;background:var(--bg)"></div>' +
      '<div style="margin:8px 0"><div style="color:var(--muted);margin-bottom:4px">说明（留痕，可空）</div>' +
      '<input id="ob-svc-reason" style="width:100%;padding:6px;border:0.5px solid var(--border);border-radius:6px;background:var(--bg)"></div>' +
      '<div style="font-size:11px;color:var(--muted);margin:8px 0">发起后流程会先做只读预检（进程/守护/插件实测），再由人工在决策卡上确认执行——不会未确认就动目标机</div>' +
      '<div style="display:flex;gap:8px;justify-content:flex-end"><button class="btn btn-o btn-sm" id="ob-svc-cancel">取消</button><button class="btn btn-d btn-sm" id="ob-svc-go">发起</button></div>';
    mask.appendChild(box);
    document.body.appendChild(mask);
    mask.addEventListener('click', function (ev) { if (ev.target === mask) mask.remove(); });
    document.getElementById('ob-svc-cancel').onclick = function () { mask.remove(); };
    document.getElementById('ob-svc-go').onclick = function () {
      var action = document.getElementById('ob-svc-act').value;
      var tsel = document.getElementById('ob-svc-tgt').value;
      var target = 'process';
      if (tsel === 'plugin') {
        var name = (document.getElementById('ob-svc-pname').value || '').trim().toLowerCase();
        if (!/^[a-z0-9_-]{1,64}$/.test(name)) { toast('插件名只允许小写字母/数字/_/-（1-64 位）', 'err'); return; }
        target = 'plugin:' + name;
      }
      var reason = document.getElementById('ob-svc-reason').value.trim();
      fetch(API + '/onboard/flow/service', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ flow_id: flowId, resource_id: f.resource_id || '', action: action, target: target, reason: reason })
      }).then(function (r) { return r.json(); }).then(function (o) {
        if (o && o.error) { toast('发起启停失败：' + o.error, 'err'); return; }
        mask.remove();
        toast('已发起启停流水线 flow-' + o.flow_id, 'ok');
        renderOnboardCenter();
        setTimeout(function () { openOnboardFlow(o.flow_id); }, 500);
      }).catch(function (e) { toast('发起启停失败：' + (e && e.message || e), 'err'); });
    };
  }).catch(function (e) { toast('读取流水线失败：' + (e && e.message || e), 'err'); });
}

function obCancel(id) {
  if (!confirm('确认取消这条接入流水线？已完成的步骤不回滚。')) return;
  fetch(API + '/onboard/flow/cancel', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ flow_id: id })
  }).then(function (r) { return r.json(); }).then(function () {
    toast('已取消', 'ok'); closeOverlay(); renderOnboardCenter();
  }).catch(function (e) { toast('取消失败：' + (e && e.message || e), 'err'); });
}

// ===================================================================
//  卸载 SAgent：接入的逆操作
//  破坏性动作不做"一键静默"——先拉这条流水线的当前状态，把「会清掉什么 / 保留什么 /
//  谁来执行」在确认框里逐条说清，再发起。确认框内容全部来自后端状态（凭据是否齐备），
//  不是前端写死的文案，避免"说是自动执行、实际要人工搬命令"的不一致
// ===================================================================
function obOffboard(flowId) {
  fetch(API + '/onboard/flow?id=' + flowId).then(function (r) { return r.json(); }).then(function (d) {
    var f = (d && d.flow) || {};
    if (!f.id) { toast('读取流水线失败', 'err'); return; }
    var lines = [];
    lines.push('资源：' + (f.resource_id || '') + (f.resource_ip ? '（' + f.resource_ip + '）' : ''));
    lines.push('');
    lines.push('目标机侧：停止 SAgent 进程 → 删除安装目录 → 由目标机自证核对（目录/进程/端口）');
    lines.push('平台侧：注销 Agent 台账与期望配置 → 撤除 vmagent 抓取登记 → 删除该资源的采集目标');
    lines.push('保留：资源对象与 SSH 凭据（卸载后可原样重新接入，不必重录凭据）');
    lines.push('');
    lines.push('执行方式：' + (f.ssh_ready
      ? '平台 ansible 自动执行（凭据齐备），完成后自动回报'
      : '⚠ 资源台账缺 SSH 凭据，将回落「下发执行包 + 人工执行回报」'));
    lines.push('');
    lines.push('卸载同样是一条可观测流水线，每一步的状态/证据/诊断都会记录在案。确认发起？');
    if (!confirm('卸载 SAgent（不可逆：会删除目标机上的 SAgent 安装目录）\n\n' + lines.join('\n'))) return;
    fetch(API + '/onboard/flow/offboard', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ flow_id: flowId, resource_id: f.resource_id || '' })
    }).then(function (r) { return r.json(); }).then(function (o) {
      if (o && o.error) { toast('发起卸载失败：' + o.error, 'err'); return; }
      toast('已发起卸载流水线 flow-' + o.flow_id, 'ok');
      renderOnboardCenter();
      setTimeout(function () { openOnboardFlow(o.flow_id); }, 500);
    }).catch(function (e) { toast('发起卸载失败：' + (e && e.message || e), 'err'); });
  }).catch(function (e) { toast('读取流水线失败：' + (e && e.message || e), 'err'); });
}
