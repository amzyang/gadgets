// ==UserScript==
// @name         react-scan 一键注入 (portless dev 站点重渲染扫描)
// @namespace    https://github.com/amzyang/gadgets
// @version      1.1.0
// @description  给所有 portless *.localhost 本地开发站点在 document-start 注入 react-scan：高亮每次重渲染、自动识别「值没变但引用变了」的不必要渲染。@require 同步执行保证先于 React 装好 DevTools 钩子；qiankun 微前端(sandbox:false)从基座进入即覆盖全部子应用。
// @author       frederick.zou
// @homepageURL  https://github.com/amzyang/gadgets/tree/main/portless-react-scan
// @supportURL   https://github.com/amzyang/gadgets/issues
// @updateURL    https://raw.githubusercontent.com/amzyang/gadgets/main/portless-react-scan/portless-react-scan.user.js
// @downloadURL  https://raw.githubusercontent.com/amzyang/gadgets/main/portless-react-scan/portless-react-scan.user.js
// @match        https://*.localhost/*
// @run-at       document-start
// @noframes
// @grant        none
// @require      https://unpkg.com/react-scan@latest/dist/auto.global.js
// ==/UserScript==

// 全部功能都在 @require 的 auto.global.js(自启动：装 DevTools 钩子 + 扫描 + 工具条)，
// 此处刻意零逻辑。时序三件套缺一不可：
//   @run-at document-start —— 先于页面任何脚本执行
//   @require               —— 代码打包进脚本体同步执行；动态 <script src> 是异步的，会和
//                             Vite dev 模块图赛跑，可能静默失效
//   @grant none            —— 跑在页面 MAIN world，钩子装到真实 window 上
// 运行时调参：控制台 window.reactScan.setOptions({...})，如 {showToolbar: false}。
// react-scan 版本：@require 指 latest，实际语义是「脚本安装/更新那一刻的最新」——
// Tampermonkey 会缓存 externals；拉新时机 = 本脚本更新时 / 重装脚本 /
// Tampermonkey 设置 → Externals 调更新间隔。
