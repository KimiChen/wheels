//go:build embed || unit

package web

import (
	"net/http"
	"path"
	"strings"
)

// Vite emits content-hashed filenames under assets/ upstream and res/ in this
// fork, so the backend can apply immutable caching without relying on a proxy.
const staticAssetsCacheControl = "public, max-age=31536000, immutable"

// isFingerprintedEmbeddedAssetPath reports whether a cleaned URL path refers to
// a Vite asset whose filename contains the default eight-character build hash.
func isFingerprintedEmbeddedAssetPath(cleanPath string) bool {
	cleanPath = strings.TrimPrefix(cleanPath, "/")
	filename := path.Base(cleanPath)
	extension := path.Ext(filename)
	stem := strings.TrimSuffix(filename, extension)
	const fingerprintLength = 8
	if extension == "" {
		return false
	}

	fingerprint := ""
	switch {
	case strings.HasPrefix(cleanPath, "assets/"):
		delimiterIndex := len(stem) - fingerprintLength - 1
		if delimiterIndex < 1 || stem[delimiterIndex] != '-' {
			return false
		}
		fingerprint = stem[delimiterIndex+1:]
	case strings.HasPrefix(cleanPath, "res/"):
		fingerprint = stem
	default:
		return false
	}

	// Vite hashes use URL-safe characters and are stable for immutable caching.
	if len(fingerprint) != fingerprintLength {
		return false
	}
	for _, char := range fingerprint {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

// applyStaticAssetCacheHeaders sets Cache-Control for long-cacheable static paths.
// index.html / SPA routes must keep no-cache and are not handled here.
func applyStaticAssetCacheHeaders(header http.Header, cleanPath string) {
	if header == nil || !isFingerprintedEmbeddedAssetPath(cleanPath) {
		return
	}
	header.Set("Cache-Control", staticAssetsCacheControl)
}
