//go:build linux && !simulator

package collect

func newCollector() Collector { return NewLinux("/") }
