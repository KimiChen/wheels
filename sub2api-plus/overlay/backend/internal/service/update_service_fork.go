package service

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 本文件集中存放 fork 的版本识别逻辑（带后缀的 fork 版本号，如 0.1.160.kim），
// 避免在上游 update_service.go 中追加整段辅助函数。
// 上游文件中的剩余引用点：
//   - UpdateInfo 结构体的 UpstreamCurrentVersion / ForkBuild 字段
//   - PerformUpdate 中的 ForkBuild 拦截
//   - parseVersion 中的一行 upstreamVersionBase 调用

// ErrManualUpdateRequired fork 构建不能直接应用上游 release 二进制，
// 需要先同步上游、重新构建再部署。
var ErrManualUpdateRequired = infraerrors.Conflict("MANUAL_UPDATE_REQUIRED", "fork builds must be synced, rebuilt, and deployed manually")

// upstreamVersionBase 去掉版本号中的 fork 后缀，返回上游语义化版本基底，
// 如 "0.1.160.kim" -> "0.1.160"。
func upstreamVersionBase(v string) string {
	v = strings.TrimSpace(strings.TrimPrefix(v, "v"))
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		if idx := strings.IndexAny(v, "-+"); idx >= 0 {
			return v[:idx]
		}
		return v
	}

	patch := parts[2]
	patchEnd := len(patch)
	for i, r := range patch {
		if r < '0' || r > '9' {
			patchEnd = i
			break
		}
	}
	if patchEnd == 0 {
		if idx := strings.IndexAny(v, "-+"); idx >= 0 {
			return v[:idx]
		}
		return v
	}
	base := strings.Join([]string{parts[0], parts[1], patch[:patchEnd]}, ".")
	if base != "" {
		return base
	}
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		return v[:idx]
	}
	return v
}

// isForkVersion 判断版本号是否带 fork 后缀（即不是纯上游版本）。
func isForkVersion(v string) bool {
	trimmed := strings.TrimSpace(strings.TrimPrefix(v, "v"))
	base := upstreamVersionBase(v)
	return base != "" && trimmed != base
}
