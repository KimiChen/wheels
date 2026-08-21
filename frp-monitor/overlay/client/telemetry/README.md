# frpc Telemetry Overlay

预留给 frpc 进程级 Telemetry 模块。当前仅有设计文档，没有实现代码。

实现必须满足：单进程单实例、首次登录后立即上报、之后每 60 秒采集、带抖动和超时，
且任何采集或上报失败都不能中断 frpc 隧道。
