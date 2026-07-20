# react-grab 一键注入（portless dev）

[![Click to Install](https://img.shields.io/badge/Tampermonkey-Click_to_Install-2ea44f?style=for-the-badge&logo=tampermonkey&logoColor=white)](https://raw.githubusercontent.com/amzyang/gadgets/main/portless-react-grab/portless-react-grab.user.js)

给所有 [portless](https://portless.sh) 的 `*.localhost` 本地开发站点在 `document-start` 注入 [react-grab](https://github.com/aidenybai/react-grab)：**悬停任意 UI 元素按 ⌘C / Ctrl+C**，把「元素 HTML + 组件名 + 源码位置」复制进剪贴板，直接粘给 Claude Code / Cursor 等 agent 定位代码。输出形如：

```txt
[<a class="ml-auto inline-block text-sm" href="#">Forgot your password?</a> in LoginForm (at components/login-form.tsx:46:19)]
```

项目仓库零侵入——不装依赖、不改入口、不加 vite 插件（官方 `npx grab@latest init` 是往项目里写代码装依赖）。

对 qiankun 微前端（`sandbox: false`）同样顺手：全页共享一个 DevTools 钩子，**从基座域名进入即覆盖全部子应用**。

## 安装

1. 装 [Tampermonkey](https://www.tampermonkey.net/)（或 Violentmonkey）。Chrome 138+ 需在扩展详情页打开「允许用户脚本」。
2. 点上方 **Click to Install** 徽章 → 确认安装。脚本带 `@updateURL`，后续更新自动拉取。
3. 访问任意 `https://*.localhost` 的 React dev 站点，悬停一个组件按 ⌘C，元素高亮且剪贴板出现组件上下文即生效（控制台可查 `window.__REACT_GRAB__`）。

## 工作机制

react-grab（底层是同作者的 bippy，与 react-scan 同源）把自己装成 `window.__REACT_DEVTOOLS_GLOBAL_HOOK__`——React 官方留给 DevTools 的接口，react-dom 启动时向钩子注册 renderer。按 ⌘C 时从悬停的 DOM 元素反查 Fiber，读出组件调用栈与 dev 构建里携带的源码位置。

官方各框架示例都要求尽早加载（Next.js 用 `strategy="beforeInteractive"`），本脚本沿用与 [portless-react-scan](../portless-react-scan) 相同的时序三件套，缺一不可：

| 配置 | 作用 |
| --- | --- |
| `@run-at document-start` | 先于页面任何脚本执行 |
| `@require index.global.js` | 代码打包进脚本体**同步**执行；动态 `<script src>` 是异步的，会和 Vite dev 模块图赛跑 |
| `@grant none` | 跑在页面 MAIN world，钩子装到真实 `window` 上 |

`index.global.js` 加载即自启动（入口顶层副作用，自动 `init()` 并挂 `window.__REACT_GRAB__`），脚本体因此刻意零逻辑。与 portless-react-scan、React DevTools 扩展共存无碍——bippy 的钩子安装是幂等的，晚到者在现有钩子上补 patch。

## 为什么不用别的方式

| 方式 | 问题 |
| --- | --- |
| 官方 CLI `npx grab@latest init` / npm 依赖 + import | 侵入业务仓库，每个仓库都要改一遍；还得自己包 `import.meta.env.DEV` 条件 |
| Inssman 等注入规则（URL 方式） | 注入的是动态 `<script src>`，异步加载与 Vite 模块图赛跑，输了就静默失效 |
| 浏览器扩展 | 官方未提供 react-grab 扩展 |

## 依赖

| 依赖 | 必需 | 说明 |
| --- | :---: | --- |
| Tampermonkey / Violentmonkey | ✅ | 用户脚本管理器 |
| react-grab@latest（`@require` 经 unpkg） | 自动 | 装机/脚本更新时解析为当时最新并缓存（~343KB），之后零网络、可离线 |

## 可调项 / 升级

- **react-grab 版本**：`@require` 指 latest，实际语义是「脚本安装/更新那一刻的最新」——Tampermonkey 会缓存 externals，不会每次页面加载都拉新。拉新时机：本脚本更新时 / 重装脚本 / Tampermonkey 设置 → Externals 调更新间隔。
- **收窄站点**：不想通配所有 `*.localhost`，在 Tampermonkey 该脚本「设置」页的 User matches / User excludes 里按站点调整，无需改脚本源。
- **排除子树**：给 DOM 子树加 `data-react-grab-ignore` 属性，命中测试会跳过它。

## 限制

- **全局监听 ⌘C / Ctrl+C**：悬停在元素上时接管复制；若与站点自身的复制行为冲突，用上面的 User excludes 排除该站点。
- **非 React 的 `*.localhost` 站点也会注入**：反查不到 Fiber 就复制不出组件上下文，代价是每次加载白执行约 343KB 脚本；介意就用 User excludes 排除。
- 只匹配 https（portless 默认 https + HTTP/2）。
- 只对装了脚本的浏览器生效——这是特性：排查工具不该进项目仓库。
