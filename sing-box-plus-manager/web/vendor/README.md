# vendored 第三方资源

前端**无构建步骤、无包管理器**（README §5），第三方库以 vendored 形式连同许可证提交，
随二进制一起发布，**运行时不走公网 CDN**。

改这里的任何文件都会让 `tests/m4/assets.rs::vendor_is_digest_locked` 变红——
这是刻意的：被依赖的东西可以变，但不能静默地变。

| 文件 | 来源 | 版本 | 许可证 |
| --- | --- | --- | --- |
| `uplot.iife.min.js` | https://github.com/leeoniya/uPlot | 1.6.32 | MIT（`uplot.LICENSE`） |
| `uplot.min.css` | 同上 | 1.6.32 | 同上 |

## 取得方式与核对（2026-09-18）

```
curl -sS https://cdn.jsdelivr.net/npm/uplot@1.6.32/dist/uPlot.iife.min.js
curl -sS https://cdn.jsdelivr.net/npm/uplot@1.6.32/dist/uPlot.min.css
curl -sS https://cdn.jsdelivr.net/npm/uplot@1.6.32/LICENSE
```

入库前做过三项核对，结论记在这里而不是只留在某次会话里：

1. **跨源摘要比对**：jsdelivr 与 unpkg 两个独立来源取到的 `uPlot.iife.min.js`
   sha256 一致（`19c8d4c6…78f1f`）。单一来源只能证明「下载成功」。
2. **无网络与动态求值**：全文件不含 `eval(`、`new Function`、`XMLHttpRequest`、
   `fetch(`、`WebSocket`、`document.cookie`、`sendBeacon`；唯一出现的 URL 是
   banner 注释里项目自己的 GitHub 地址。
3. **许可证随文件提交**：MIT，见 `uplot.LICENSE`。

## 为什么选 uPlot

canvas 绘制、零依赖、约 50 KB。`series.value` 回调能拿到数据点下标，
所以 **tooltip 可以回查服务端返回的十进制字符串**，而不是把画图用的浮点当精确流量值
（README §4.11 的第一条纪律）。`setSize()` 支持面板从隐藏变可见时重新量尺寸。
