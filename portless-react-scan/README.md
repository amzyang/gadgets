# react-scan 一键注入（portless dev）

[![Click to Install](https://img.shields.io/badge/Tampermonkey-Click_to_Install-2ea44f?style=for-the-badge&logo=tampermonkey&logoColor=white)](https://raw.githubusercontent.com/amzyang/gadgets/main/portless-react-scan/portless-react-scan.user.js)

给所有 [portless](https://portless.sh) 的 `*.localhost` 本地开发站点在 `document-start` 注入 [react-scan](https://github.com/aidenybai/react-scan)：高亮每次重渲染、自动识别「值没变但引用变了」的**不必要渲染**，页面右下常驻工具条（props diff / FPS / 开关）。项目仓库零侵入——不装依赖、不改入口、不加 vite 插件。

对 qiankun 微前端（`sandbox: false`）尤其顺手：全页共享一个 DevTools 钩子，**从基座域名进入即覆盖全部子应用**（React 18 / 19 混跑也行，钩子按 renderer 区分）。

## 安装

1. 装 [Tampermonkey](https://www.tampermonkey.net/)（或 Violentmonkey）。Chrome 138+ 需在扩展详情页打开「允许用户脚本」。
2. 点上方 **Click to Install** 徽章 → 确认安装。脚本带 `@updateURL`，后续更新自动拉取。
3. 访问任意 `https://*.localhost` 的 React dev 站点，右下出现 react-scan 工具条即生效。

## 工作机制

react-scan（底层是同作者的 bippy）把自己装成 `window.__REACT_DEVTOOLS_GLOBAL_HOOK__`——React 官方留给 DevTools 的接口。react-dom 启动时向钩子注册 renderer，之后每次 commit 回调 `onCommitFiberRoot`；react-scan 借此遍历 Fiber 树，对比 fiber 与 alternate 的 props/state/context，把不必要渲染用 canvas 高亮出来。

钩子必须**先于 React** 就位，本脚本靠三件套保证时序，缺一不可：

| 配置 | 作用 |
| --- | --- |
| `@run-at document-start` | 先于页面任何脚本执行 |
| `@require auto.global.js` | 代码打包进脚本体**同步**执行；动态 `<script src>` 是异步的，会和 Vite dev 模块图赛跑 |
| `@grant none` | 跑在页面 MAIN world，钩子装到真实 `window` 上 |

装了 React DevTools 扩展时时序还会进一步放宽：RDT 在 document_start 已装好钩子，React 必然注册上去，react-scan 晚到也只是在现有钩子上补 patch——官方 react-scan 扩展与 RDT 共存就是这个原理。

## 为什么不用别的方式

| 方式 | 问题 |
| --- | --- |
| npm 依赖 + 项目内 import | 侵入业务仓库；动态 import 晚于 react-dom 初始化会失效，静态 import 又无法被 prod 构建死代码消除 |
| 官方 CLI | 0.5.x 起只剩 `init`（往项目写代码装依赖），「传 URL 开浏览器」模式停留在 0.4.3，且拉起的是无登录态的全新浏览器 |
| 官方浏览器扩展 | 可用，但不可钉版本、不能按 `*.localhost` 精确圈定站点 |
| Inssman 等注入规则（URL 方式） | 注入的是动态 `<script src>`，异步加载与 Vite 模块图赛跑，输了就静默失效 |

## 依赖

| 依赖 | 必需 | 说明 |
| --- | :---: | --- |
| Tampermonkey / Violentmonkey | ✅ | 用户脚本管理器 |
| react-scan@0.5.7（`@require` 经 unpkg） | 自动 | 安装脚本时下载一次（~100KB）并缓存，之后零网络、可离线 |

## 可调项 / 升级

- **运行时选项**：控制台 `window.reactScan.setOptions({...})`，如 `{showToolbar: false}`；工具条自身也有开关。
- **升级 react-scan**：改脚本头 `@require` 里的版本号，同时 bump `@version` 触发已装机器自动更新。
- **收窄站点**：不想通配所有 `*.localhost`，在 Tampermonkey 该脚本「设置」页的 User matches / User excludes 里按站点调整，无需改脚本源。

## 限制

- **非 React 的 `*.localhost` 站点也会注入**：实测（对纯 JSON 后端页注入）完全静默——不出工具条、不建 canvas、`__REACT_SCAN__` 都不初始化，唯一代价是每次加载白执行约 100KB 脚本；介意就用上面的 User excludes 排除。
- 只匹配 https（portless 默认 https + HTTP/2）。
- 常开有每次 commit 遍历 Fiber 树的开销，日常开发有感知的话，用完在 Tampermonkey 面板关掉脚本。
- 只对装了脚本的浏览器生效——这是特性：排查工具不该进项目仓库。
