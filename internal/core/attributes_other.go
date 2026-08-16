//go:build !linux && !darwin

package core

import "os"

func applyPlatformAttributes(os.FileInfo, *Attributes) {}
