//go:build !linux && !darwin

package configuration

import "os"

func backupOwnerAndLinksForCheckpoint(os.FileInfo) bool { return false }
