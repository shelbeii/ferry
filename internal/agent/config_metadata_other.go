//go:build !linux

package agent

import "os"

func configMetadata(os.FileInfo) (string, bool) { return "", false }

func preserveConfigMetadata(*os.File, *os.File, bool) error { return nil }
