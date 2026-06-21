// ==UserScript==
// @name         飞书代码块 ASCII 图零偏差对齐 (CJK 2:1 + 终端歧义宽度)
// @namespace    https://github.com/amzyang/gadgets
// @version      1.0.0
// @description  将飞书文档/Wiki 代码块字体替换为与 kitty 终端网格一致的等宽字体(Sarasa Term/Fixed SC)，修复 box-drawing 图中英文混排错位、右边框锯齿、竖线断裂，做到逐像素复刻终端
// @author       frederick.zou
// @homepageURL  https://github.com/amzyang/gadgets/tree/main/feishu-codeblock-mono-fix
// @supportURL   https://github.com/amzyang/gadgets/issues
// @updateURL    https://raw.githubusercontent.com/amzyang/gadgets/main/feishu-codeblock-mono-fix/feishu-codeblock-mono-fix.user.js
// @downloadURL  https://raw.githubusercontent.com/amzyang/gadgets/main/feishu-codeblock-mono-fix/feishu-codeblock-mono-fix.user.js
// @match        https://*.feishu.cn/*
// @match        https://*.feishu.net/*
// @match        https://*.larksuite.com/*
// @run-at       document-start
// @grant        GM_addStyle
// ==/UserScript==

(function () {
  'use strict';
  const CSS = `
    /* 作用域严格限定在代码块内：飞书正文段落也用 .ace-line，绝不能误伤 */
    .code-block-zone-container,
    .code-block-zone-container .code-line-wrapper,
    .code-block-zone-container .ace-line,
    .code-block-zone-container [data-string] {
      font-family:
        'Sarasa Term SC','Sarasa Fixed SC',  /* 像素级：歧义符号 →▶① 按 1 格，与终端一致 */
        'Sarasa Mono SC',                     /* 退路：仍 2:1，但 →▶① 偏 1 格(≈7px) */
        monospace !important;
      letter-spacing: 0 !important;
      font-variant-ligatures: none !important;
      font-feature-settings: "liga" 0, "calt" 0 !important;
    }
    /* 收紧行高让竖线 │ ┌ └ 连实(还原终端 line-height≈1 观感) */
    .code-block-zone-container .ace-line {
      line-height: 1.2 !important;   /* 可调：竖线更连实→1.15；更宽松→1.5 */
    }
  `;
  if (typeof GM_addStyle === 'function') GM_addStyle(CSS);
  else {
    const s = document.createElement('style');
    s.textContent = CSS;
    (document.head || document.documentElement).appendChild(s);
  }
})();
