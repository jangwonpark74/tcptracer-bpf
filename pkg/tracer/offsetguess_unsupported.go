// +build !linux_bpf

package tracer

import (
	"github.com/iovisor/gobpf/elf"
)

func guess(b *elf.Module) error {
	return ErrNotImplemented
}
