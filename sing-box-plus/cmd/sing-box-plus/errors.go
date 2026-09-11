package main

import E "github.com/sagernet/sing/common/exceptions"

// errStatsRemoved：重载时把 user_stats 整块删掉等于把统计从硬依赖降级成可选项，
// 而进程已经按「有统计」的前提运行（README §4.6 第 6 条与 §4.4 的重载语义）。
var errStatsRemoved = E.New("重载不得移除 user_stats 配置：统计是硬依赖")
