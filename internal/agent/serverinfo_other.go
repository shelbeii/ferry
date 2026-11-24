//go:build !linux

package agent

import "errors"

func unameInfo() (kernel, arch, nodename string) { return "", "", "" }

func statMount(string) (mountStat, error) { return mountStat{}, errors.New("仅支持 Linux") }
