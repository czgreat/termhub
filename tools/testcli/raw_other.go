//go:build !windows && !linux

package main

func rawInput() func() { return func() {} }
