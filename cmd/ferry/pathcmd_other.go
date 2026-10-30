//go:build !windows

package main

import "errors"

const userPathSupported = false

func userPath() (string, error) { return "", errors.New("只支持 Windows") }

func setUserPath(string) error { return errors.New("只支持 Windows") }

func setPathChoice(bool) {}
