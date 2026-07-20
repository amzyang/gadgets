// ==UserScript==
// @name         react-grab 一键注入 (portless dev 站点元素上下文复制)
// @namespace    https://github.com/amzyang/gadgets
// @version      1.0.0
// @description  给所有 portless *.localhost 本地开发站点在 document-start 注入 react-grab：悬停任意元素按 ⌘C / Ctrl+C，复制「元素 HTML + 组件名 + 源码位置」直接粘给 AI agent。@require 同步执行保证先于 React 装好 DevTools 钩子；qiankun 微前端(sandbox:false)从基座进入即覆盖全部子应用。
// @author       frederick.zou
// @homepageURL  https://github.com/amzyang/gadgets/tree/main/portless-react-grab
// @supportURL   https://github.com/amzyang/gadgets/issues
// @updateURL    https://raw.githubusercontent.com/amzyang/gadgets/main/portless-react-grab/portless-react-grab.user.js
// @downloadURL  https://raw.githubusercontent.com/amzyang/gadgets/main/portless-react-grab/portless-react-grab.user.js
// @match        https://*.localhost/*
// @run-at       document-start
// @noframes
// @grant        none
// @require      https://unpkg.com/react-grab@latest/dist/index.global.js
// ==/UserScript==

// 全部功能都在 @require 的 index.global.js(自启动：装 DevTools 钩子 + ⌘C 监听 + 高亮 overlay)，
// 此处刻意零逻辑。时序三件套缺一不可：
//   @run-at document-start —— 先于页面任何脚本执行
//   @require               —— 代码打包进脚本体同步执行；动态 <script src> 是异步的，会和
//                             Vite dev 模块图赛跑，可能静默失效
//   @grant none            —— 跑在页面 MAIN world，钩子装到真实 window 上
// 运行时：API 挂在 window.__REACT_GRAB__；与 portless-react-scan 共存无碍——同底层 bippy，
// 钩子安装幂等。不想在某站点启用，用 Tampermonkey 的 User excludes 或面板开关。
// react-grab 版本：@require 指 latest，实际语义是「脚本安装/更新那一刻的最新」——
// Tampermonkey 会缓存 externals；拉新时机 = 本脚本更新时 / 重装脚本 /
// Tampermonkey 设置 → Externals 调更新间隔。
