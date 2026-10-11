package apple

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// architectureHeader is the longest universal header: 32 64-bit entries.
const architectureHeader = 8 + 32*32

// architectures names the CPU architectures declared at the start of a Mach-O
// file, sorted. It returns nil for any other bytes.
func architectures(header []byte) []string {
	if len(header) < 8 {
		return nil
	}
	magic := binary.BigEndian.Uint32(header[:4])
	var cpus []uint32
	if magic == 0xcafebabe || magic == 0xcafebabf {
		count, entry := int(binary.BigEndian.Uint32(header[4:8])), 20
		if magic == 0xcafebabf {
			entry = 32
		}
		if count == 0 || count > 32 || len(header) < 8+entry*count {
			return nil
		}
		for i := range count {
			cpus = append(cpus, binary.BigEndian.Uint32(header[8+i*entry:]))
		}
	} else {
		order, _, err := machoOrder(magic)
		if err != nil {
			return nil
		}
		cpus = []uint32{order.Uint32(header[4:8])}
	}
	names := make([]string, 0, len(cpus))
	for _, cpu := range cpus {
		name, known := map[uint32]string{7: "i386", 0x01000007: "x86_64", 12: "arm", 0x0100000c: "arm64", 0x0200000c: "arm64_32", 18: "ppc", 0x01000012: "ppc64"}[cpu]
		if !known {
			name = fmt.Sprintf("cpu-%d", cpu)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}
