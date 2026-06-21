// ==UserScript==
// @name         飞书代码块 ASCII 图零偏差对齐 (CJK 2:1 + 终端歧义宽度)
// @namespace    https://github.com/amzyang/gadgets
// @version      1.1.0
// @description  把飞书文档/Wiki 代码块字体换成与 kitty 终端 wcwidth 网格一致的等宽字体(Sarasa Term/Fixed SC)，修复 box-drawing 图中英文混排错位、右边框锯齿、竖线断裂。本地无字体时自动走国内友好 CDN(ZeoSeven·Sarasa Mono SC)兜底并提示。
// @author       frederick.zou
// @homepageURL  https://github.com/amzyang/gadgets/tree/main/feishu-codeblock-mono-fix
// @supportURL   https://github.com/amzyang/gadgets/issues
// @updateURL    https://raw.githubusercontent.com/amzyang/gadgets/main/feishu-codeblock-mono-fix/feishu-codeblock-mono-fix.user.js
// @downloadURL  https://raw.githubusercontent.com/amzyang/gadgets/main/feishu-codeblock-mono-fix/feishu-codeblock-mono-fix.user.js
// @match        https://*.feishu.cn/*
// @match        https://*.feishu.net/*
// @match        https://*.larksuite.com/*
// @run-at       document-start
// @noframes
// @grant        GM_addStyle
// ==/UserScript==

(function () {
  'use strict';

  /* ===== 可调项 ===== */
  // 行高：收紧让竖线 │ ┌ └ 连实(接近终端)。设为 '' 可关闭——飞书「常驻 contenteditable」，
  // 无法可靠区分阅读/编辑态，编辑时选区高亮浮层可能与文字轻微错位，介意就置空。
  const LINE_HEIGHT = '1.2';
  // 国内友好分包 CDN(ZeoSeven)，仅在本地无 Sarasa 时挂载兜底；其 @font-face 自带 local() 优先。
  const CDN_CSS = 'https://fontsapi.zeoseven.com/159/main/result.css'; // = Sarasa Mono SC

  /* ===== 1. 字体覆盖(本地优先：Term/Fixed 像素级 → Mono SC 2:1 → monospace) ===== */
  // 作用域严格限定在 .code-block-zone-container 内：飞书正文段落也用 .ace-line，不能误伤。
  GM_addStyle(`
    .code-block-zone-container,
    .code-block-zone-container .code-line-wrapper,
    .code-block-zone-container .ace-line,
    .code-block-zone-container [data-string] {
      font-family: 'Sarasa Term SC','Sarasa Fixed SC','Sarasa Mono SC',monospace !important;
      letter-spacing: 0 !important;
      font-variant-ligatures: none !important;
      font-feature-settings: "liga" 0, "calt" 0 !important;
    }
    ${LINE_HEIGHT ? `.code-block-zone-container .ace-line { line-height: ${LINE_HEIGHT} !important; }` : ''}
  `);

  /* ===== 2. 本地无 Sarasa → 挂载 CDN 兜底 + 一次性提示 ===== */
  function localSarasaInstalled() {
    const measure = (family) => {
      const s = document.createElement('span');
      s.style.cssText = 'position:absolute;left:-9999px;visibility:hidden;white-space:pre;font-size:40px';
      s.style.fontFamily = family;
      s.textContent = 'MMMMMMMMMMMMMMMMMMMM';
      document.documentElement.appendChild(s);
      const w = s.getBoundingClientRect().width;
      s.remove();
      return w;
    };
    // 命中本地 Sarasa(任一变体) 时拉丁步进为 0.5em，明显窄于系统 monospace(≈0.6em)
    return Math.abs(
      measure("'Sarasa Term SC','Sarasa Fixed SC','Sarasa Mono SC',monospace") - measure('monospace')
    ) > 1;
  }

  function toast(text) {
    if (sessionStorage.getItem('__feishu_mono_cdn_noted')) return;
    sessionStorage.setItem('__feishu_mono_cdn_noted', '1');
    const box = document.createElement('div');
    box.style.cssText = [
      'position:fixed', 'right:16px', 'bottom:16px', 'z-index:2147483647',
      'max-width:300px', 'padding:12px 14px', 'border-radius:10px',
      'background:#1f2329', 'color:#fff', 'font:13px/1.5 system-ui,-apple-system,sans-serif',
      'box-shadow:0 6px 24px rgba(0,0,0,.28)', 'opacity:0', 'transition:opacity .25s',
    ].join(';');
    box.textContent = text;
    const close = document.createElement('span');
    close.textContent = '✕';
    close.style.cssText = 'float:right;margin-left:10px;cursor:pointer;opacity:.6';
    close.onclick = () => box.remove();
    box.prepend(close);
    document.body.appendChild(box);
    requestAnimationFrame(() => { box.style.opacity = '1'; });
    setTimeout(() => { box.style.opacity = '0'; setTimeout(() => box.remove(), 300); }, 10000);
  }

  function ensureFont() {
    if (localSarasaInstalled()) return; // 本地有，最优：零网络、零开销
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = CDN_CSS;
    (document.head || document.documentElement).appendChild(link);
    toast('飞书代码块对齐：本地未装 Sarasa，已启用 CDN 字体(ZeoSeven · Sarasa Mono SC)。装 Sarasa Term SC 可更快且像素级：brew install --cask font-sarasa-gothic');
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', ensureFont, { once: true });
  } else {
    ensureFont();
  }
})();
