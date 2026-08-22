package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveVersionAppendsSubVersion(t *testing.T) {
	require.Equal(t, "0.1.138.kim", resolveVersion("", "0.1.138", ".kim"))
	require.Equal(t, "0.1.138.kim", resolveVersion("0.1.138", "0.1.137", ".kim"))
	require.Equal(t, "0.1.138.kim", resolveVersion("0.1.138.kim", "0.1.137", ".kim"))
}

func TestResolveVersionFallback(t *testing.T) {
	require.Equal(t, "0.0.0-dev.kim", resolveVersion("", "", ".kim"))
	require.Equal(t, "0.1.138", resolveVersion("", "0.1.138", ""))
}
