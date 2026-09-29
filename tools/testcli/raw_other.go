//go:build !windows

package main

func rawInput() func() { return func() {} }
