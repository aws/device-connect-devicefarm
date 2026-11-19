// Command makefat writes a universal (fat) Mach-O from thin Mach-O slices.
//
// Usage: makefat -o <output> <thin slice> <thin slice>...
//
// Go emits no fat files and lipo is absent on the Linux build hosts, so this
// tool assembles the macOS universal binary.
package main

import (
	"debug/macho"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
)

// Fat header and arch table are big-endian; slices start on 1 << alignBits.
const (
	alignBits     = 14
	sliceAlign    = 1 << alignBits
	fatHeaderSize = 8
	fatArchSize   = 20
)

// slice is one thin input and where it lands in the output.
type slice struct {
	path   string
	cpu    macho.Cpu
	subCpu uint32
	offset uint32
	size   uint32
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stderr))
}

func realMain(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("makefat", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("o", "", "path of the universal binary to write")
	if err := flags.Parse(args); err != nil || *output == "" || flags.NArg() < 2 {
		fmt.Fprintln(stderr, "usage: makefat -o <output> <thin slice> <thin slice>...")
		return 1
	}
	if err := run(*output, flags.Args()); err != nil {
		fmt.Fprintln(stderr, "makefat:", err)
		return 1
	}
	return 0
}

// run writes to <output>.tmp and renames only after the file re-parses with
// every input architecture, so a failed run leaves no universal file.
func run(output string, inputs []string) error {
	slices, err := readSlices(inputs)
	if err != nil {
		return err
	}
	tmp := output + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	err = writeFat(out, slices)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = verifyFat(tmp, cpus(slices))
	}
	if err == nil {
		err = os.Rename(tmp, output)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// readSlices reads each thin header and lays the slices out after the arch table.
func readSlices(paths []string) ([]slice, error) {
	slices := make([]slice, 0, len(paths))
	seen := map[macho.Cpu]bool{}
	offset := uint64(fatHeaderSize + fatArchSize*len(paths))
	for _, path := range paths {
		thin, err := macho.Open(path)
		if err != nil {
			return nil, fmt.Errorf("%s is not a thin Mach-O: %v", path, err)
		}
		cpu, subCpu := thin.Cpu, thin.SubCpu
		thin.Close()
		if seen[cpu] {
			return nil, fmt.Errorf("duplicate architecture %s in %s", cpu, path)
		}
		seen[cpu] = true
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		offset = alignUp(offset, sliceAlign)
		if offset+uint64(info.Size()) > math.MaxUint32 {
			return nil, fmt.Errorf("%s does not fit a 32-bit fat header", path)
		}
		slices = append(slices, slice{path: path, cpu: cpu, subCpu: subCpu, offset: uint32(offset), size: uint32(info.Size())})
		offset += uint64(info.Size())
	}
	return slices, nil
}

// writeFat writes the header, the arch table, then each slice at its offset.
func writeFat(out io.Writer, slices []slice) error {
	header := []uint32{macho.MagicFat, uint32(len(slices))}
	for _, s := range slices {
		header = append(header, uint32(s.cpu), s.subCpu, s.offset, s.size, alignBits)
	}
	if err := binary.Write(out, binary.BigEndian, header); err != nil {
		return err
	}
	written := uint32(fatHeaderSize + fatArchSize*len(slices))
	for _, s := range slices {
		if _, err := out.Write(make([]byte, s.offset-written)); err != nil {
			return err
		}
		in, err := os.Open(s.path)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, in)
		in.Close()
		if err != nil {
			return err
		}
		if n != int64(s.size) {
			return fmt.Errorf("%s changed size while copying", s.path)
		}
		written = s.offset + s.size
	}
	return nil
}

// verifyFat re-parses path and fails unless every wanted Cpu is present exactly once.
func verifyFat(path string, want []macho.Cpu) error {
	fat, err := macho.OpenFat(path)
	if err != nil {
		return fmt.Errorf("%s is not a fat Mach-O: %v", path, err)
	}
	defer fat.Close()
	count := map[macho.Cpu]int{}
	for _, arch := range fat.Arches {
		count[arch.Cpu]++
	}
	for _, cpu := range want {
		if n := count[cpu]; n == 0 {
			return fmt.Errorf("%s is missing architecture %s", path, cpu)
		} else if n > 1 {
			return fmt.Errorf("duplicate architecture %s in %s", cpu, path)
		}
	}
	return nil
}

func cpus(slices []slice) []macho.Cpu {
	out := make([]macho.Cpu, len(slices))
	for i, s := range slices {
		out[i] = s.cpu
	}
	return out
}

func alignUp(n, to uint64) uint64 {
	return (n + to - 1) / to * to
}
