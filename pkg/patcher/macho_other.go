//go:build !darwin

package patcher

func prepareSwappedBinary(_, _ string) error { return nil }
func afterBinaryWrite(_ string)              {}
