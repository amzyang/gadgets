# 飞书代码块 ASCII 图对齐修复

[![Click to Install](https://img.shields.io/badge/Tampermonkey-Click_to_Install-2ea44f?style=for-the-badge&logo=tampermonkey&logoColor=white)](https://raw.githubusercontent.com/amzyang/gadgets/main/feishu-codeblock-mono-fix/feishu-codeblock-mono-fix.user.js)

把飞书文档 / Wiki 代码块的字体换成「与终端 wcwidth 网格一致」的等宽字体，让用 box-drawing 字符（`─│┌┐└┘┼▶▼`）+ 中文画的 ASCII 架构图，在网页上像终端里一样**逐像素对齐**。本地装了 Sarasa 用本地（秒开、像素级），没装则**自动走国内友好 CDN 兜底**。

## 效果对比

| 修复前（中英错位 · 右边框锯齿 · 竖线断裂） | 修复后（零像素偏差 · 框线闭合） |
| :---: | :---: |
| <img src="before.png" width="430" alt="修复前"> | <img src="after.png" width="430" alt="修复后"> |

## 安装

1. 装 [Tampermonkey](https://www.tampermonkey.net/)（或 Violentmonkey）。
2. 点上方 **Click to Install** 徽章 → Tampermonkey 自动弹出安装页 → 确认。
3. 刷新飞书文档即可。脚本带 `@updateURL`，后续更新自动拉取。
4. （**推荐但非必需**）本地装 Sarasa Term SC，获得像素级 + 零网络：
   ```bash
   brew install --cask font-sarasa-gothic   # macOS；其他平台见 be5invis/Sarasa-Gothic releases
   ```
   不装也能用——脚本会自动加载 CDN 字体（见下）。

## 工作机制：本地优先，自动降级

脚本字体栈 `Sarasa Term SC → Sarasa Fixed SC → Sarasa Mono SC → monospace`，启动时探测本地是否装了 Sarasa：

| 你的环境 | 效果 | 网络 |
| --- | --- | --- |
| 本地有 **Sarasa Term / Fixed SC** | **像素级零偏差**（歧义符号 `→▶①` 也按 1 格）| 零 |
| 本地只有 **Sarasa Mono SC** | 2:1 对齐（`→▶①` 偏 1 格 ≈7px）| 零 |
| **本地无 Sarasa** | 自动挂载 [ZeoSeven](https://fonts.zeoseven.com/items/159/) 的 **Sarasa Mono SC**（2:1），右下角一次性提示「已走 CDN」 | 按需分包，国内友好 |

> CDN 选 ZeoSeven（`fontsapi.zeoseven.com`，**国内访问快**，jsDelivr 在国内不稳故不用）；其 `@font-face` 自带 `local()` 优先 + `unicode-range` 分包 + `font-display:swap`，只下载用到的几十 KB。

## 依赖

| 依赖 | 必需 | 说明 |
| --- | :---: | --- |
| Tampermonkey / Violentmonkey | ✅ | 用户脚本管理器 |
| Sarasa 字体（本地） | ⬜ 推荐 | 装 **Term/Fixed SC** 得像素级 + 秒开；不装则自动走 CDN（仍 2:1）|

## 它修复了什么

飞书代码块用的是 `SourceCodeProMac`——**只含拉丁的等宽字体，字体栈里没有任何中文字体**。在它下面实测逐字步进宽度：

| 字符类 | 相对 ASCII 宽度 | ASCII 图/终端的假设 |
| --- | --- | --- |
| ASCII / box-drawing | 1.0 | 1 格 ✓ |
| **中文** | **1.667** | **2 格** ✗ |
| 歧义宽度符号 `→ ▶ ① ` | 2.0（CJK 字体下） | 1 格 ✗ |

- **主因**：中文宽度是 ASCII 的 1.667 倍而非 2 倍（中文 fallback 到全角 PingFang，1.0em ÷ 拉丁 0.6em）。图按「中文 = 2 个英文格」绘制，每个中文把后面内容左拽 ≈2.8px，**右边框逐行累计错位最高 ~36px（≈5 格锯齿）**。
- **像素级残差**：图里大量用 `→`（及 `▶▼▲①②③`），这些是东亚 *Ambiguous* 宽度字符——终端按 1 格、多数 CJK 字体按 2 格。

**为什么终端好、网页坏**：终端（kitty 等）对齐**靠等宽字符网格、不靠字体**——按 east-asian-width 强制中文占 2 cell、歧义符号占 1 cell。浏览器按字形 advance 比例排版、没有网格，所以必须换一款「每类字符 advance 恰好等于终端格数」的字体才能复刻。

## 原理：为什么是 Sarasa Term / Fixed

| 字符 | Sarasa **Mono** SC | **Sarasa Term / Fixed SC** | kitty 终端 |
| --- | :---: | :---: | :---: |
| `→ ▶ ▼ ▲ ① ② ③`（歧义宽度）| 2 格 ✗ | **1 格 ✓** | 1 格 |
| 中文 | 2 格 | 2 格 | 2 格 |
| ASCII / box-drawing | 1 格 | 1 格 | 1 格 |

只有 **Term / Fixed 变体**把歧义宽度符号按 1 格渲染，每类字符的格数都与终端一致 → 零偏差。实测顶部外框右边框逐行 x 坐标极差：**默认 33.6px → Sarasa Mono SC 7px → Sarasa Term SC 0px**。

## 可调项（脚本顶部常量）

- **`LINE_HEIGHT`**（默认 `'1.2'`）：收紧行高让竖线连实。设为 `''` 关闭——飞书代码块**常驻 `contenteditable`**，脚本无法可靠区分阅读/编辑态，所以行高不能只在阅读态生效；编辑时若选区浮层错位，置空即可。
- **`CDN_CSS`**：兜底字体地址，默认 ZeoSeven 的 Sarasa Mono SC（159）。
- **范围**：`@match https://*.feishu.cn/*` 作用于所有飞书代码块（顺带修复含中文注释的代码）；`@noframes` 不在内嵌 iframe 重复运行。
- **彻底无依赖**：把源文里的 `→▶▼▲①②③` 换成 ASCII（`->`、`>`、`v`、`^`、`(1)`…）这些无歧义 1 格字符，则任何 2:1 等宽字体都能零偏差。

## 限制

- **只对装了脚本的浏览器生效**：同事打开同一篇文档仍是错位的。要让所有人都正常，须改源（转图片 / 飞书原生流程图）。
- **编辑模式**：`LINE_HEIGHT` 非空时，飞书编辑态光标/选区浮层可能与文字轻微错位（按行高算坐标）。纯查看无影响。
- **CDN 兜底的 CSP**：已验证飞书允许加载外部字体样式表；若你司飞书配了更严的 `font-src` CSP 拦截了第三方字体，CDN 兜底会失效——此时本地装 Sarasa 即可。
