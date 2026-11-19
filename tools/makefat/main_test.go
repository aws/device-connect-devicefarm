package main

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeThinSlice writes a minimal 64-bit Mach-O: a header with no load
// commands followed by payload, enough for debug/macho to parse.
func writeThinSlice(t *testing.T, dir, name string, cpu macho.Cpu, typ macho.Type, payload []byte) string {
	t.Helper()
	var buf bytes.Buffer
	fields := []uint32{macho.Magic64, uint32(cpu), 0, uint32(typ), 0, 0, 0, 0}
	if err := binary.Write(&buf, binary.LittleEndian, fields); err != nil {
		t.Fatal(err)
	}
	buf.Write(payload)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func payload(fill byte, n int) []byte {
	return bytes.Repeat([]byte{fill}, n)
}

// mustRun goes through the CLI entrypoint and returns the parsed output.
func mustRun(t *testing.T, output string, inputs ...string) *macho.FatFile {
	t.Helper()
	var stderr bytes.Buffer
	if code := realMain(append([]string{"-o", output}, inputs...), &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	fat, err := macho.OpenFat(output)
	if err != nil {
		t.Fatalf("OpenFat: %v", err)
	}
	t.Cleanup(func() { fat.Close() })
	return fat
}

func assertNoOutput(t *testing.T, output string) {
	t.Helper()
	for _, path := range []string{output, output + ".tmp"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after a failed run (stat err %v)", path, err)
		}
	}
}

// limitWriter fails once more than limit bytes have been written.
type limitWriter struct {
	limit   int
	written int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.limit {
		return 0, errors.New("disk full")
	}
	w.written += len(p)
	return len(p), nil
}

func TestGivenTwoThinSlicesWhenWrittenThenFatFileListsBothArchitectures(t *testing.T) {
	dir := t.TempDir()
	amd64 := writeThinSlice(t, dir, "amd64", macho.CpuAmd64, macho.TypeExec, payload('a', 100))
	arm64 := writeThinSlice(t, dir, "arm64", macho.CpuArm64, macho.TypeExec, payload('b', 200))
	output := filepath.Join(dir, "universal")

	fat := mustRun(t, output, amd64, arm64)

	got := map[macho.Cpu]bool{}
	for _, arch := range fat.Arches {
		got[arch.Cpu] = true
	}
	if len(fat.Arches) != 2 || !got[macho.CpuAmd64] || !got[macho.CpuArm64] {
		t.Fatalf("arches = %v, want amd64 and arm64", fat.Arches)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("output mode %v is not executable", info.Mode())
	}
	if _, err := os.Stat(output + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestGivenSlicesWhenWrittenThenEachSliceStartsAtA16KiBBoundaryAndIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	// Sizes straddle the alignment so the second slice needs real padding.
	inputs := []string{
		writeThinSlice(t, dir, "amd64", macho.CpuAmd64, macho.TypeExec, payload('a', sliceAlign+123)),
		writeThinSlice(t, dir, "arm64", macho.CpuArm64, macho.TypeExec, payload('b', 77)),
	}
	output := filepath.Join(dir, "universal")

	fat := mustRun(t, output, inputs...)

	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for i, arch := range fat.Arches {
		if arch.Offset%sliceAlign != 0 || arch.Align != alignBits {
			t.Errorf("slice %d: offset %d align %d, want a 16 KiB boundary and align %d", i, arch.Offset, arch.Align, alignBits)
		}
		want, err := os.ReadFile(inputs[i])
		if err != nil {
			t.Fatal(err)
		}
		end := int(arch.Offset) + int(arch.Size)
		if end > len(raw) || !bytes.Equal(raw[arch.Offset:end], want) {
			t.Errorf("slice %d bytes differ from %s", i, inputs[i])
		}
	}
	last := fat.Arches[len(fat.Arches)-1]
	if int(last.Offset)+int(last.Size) != len(raw) {
		t.Errorf("file length %d, want it to end with the last slice at %d", len(raw), int(last.Offset)+int(last.Size))
	}
}

func TestGivenDuplicateArchitectureWhenWrittenThenFails(t *testing.T) {
	dir := t.TempDir()
	first := writeThinSlice(t, dir, "first", macho.CpuAmd64, macho.TypeExec, payload('a', 10))
	second := writeThinSlice(t, dir, "second", macho.CpuAmd64, macho.TypeExec, payload('b', 10))
	output := filepath.Join(dir, "universal")
	var stderr bytes.Buffer

	code := realMain([]string{"-o", output, first, second}, &stderr)

	if code != 1 || !strings.Contains(stderr.String(), "makefat: duplicate architecture CpuAmd64 in "+second) {
		t.Fatalf("exit %d stderr %q, want exit 1 and duplicate architecture naming %s", code, stderr.String(), second)
	}
	assertNoOutput(t, output)
}

func TestGivenNonMachOInputWhenWrittenThenFailsNamingTheFile(t *testing.T) {
	dir := t.TempDir()
	arm64 := writeThinSlice(t, dir, "arm64", macho.CpuArm64, macho.TypeExec, payload('b', 10))
	text := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(text, []byte("not a binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "universal")

	err := run(output, []string{arm64, text})

	if err == nil || !strings.Contains(err.Error(), text+" is not a thin Mach-O") {
		t.Fatalf("err = %v, want not a thin Mach-O naming %s", err, text)
	}
	assertNoOutput(t, output)
}

func TestGivenSlicesOfDifferentFileTypesWhenWrittenThenFailsAndRemovesTheTempFile(t *testing.T) {
	dir := t.TempDir()
	// debug/macho rejects a fat file whose slices differ in type, so the
	// verification step fails after the temp file was written.
	exec := writeThinSlice(t, dir, "exec", macho.CpuAmd64, macho.TypeExec, payload('a', 10))
	dylib := writeThinSlice(t, dir, "dylib", macho.CpuArm64, macho.TypeDylib, payload('b', 10))
	output := filepath.Join(dir, "universal")

	err := run(output, []string{exec, dylib})

	if err == nil || !strings.Contains(err.Error(), output+".tmp is not a fat Mach-O") {
		t.Fatalf("err = %v, want not a fat Mach-O naming the temp file", err)
	}
	assertNoOutput(t, output)
}

func TestGivenFatFileMissingAnArchitectureWhenVerifiedThenFails(t *testing.T) {
	dir := t.TempDir()
	amd64 := writeThinSlice(t, dir, "amd64", macho.CpuAmd64, macho.TypeExec, payload('a', 10))
	output := filepath.Join(dir, "universal")
	if err := run(output, []string{amd64}); err != nil {
		t.Fatal(err)
	}

	err := verifyFat(output, []macho.Cpu{macho.CpuAmd64, macho.CpuArm64})

	if err == nil || !strings.Contains(err.Error(), "missing architecture CpuArm64") {
		t.Fatalf("err = %v, want missing architecture CpuArm64", err)
	}
}

func TestGivenWriterThatFailsWhenWritingThenTheErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	slices, err := readSlices([]string{
		writeThinSlice(t, dir, "amd64", macho.CpuAmd64, macho.TypeExec, payload('a', 10)),
		writeThinSlice(t, dir, "arm64", macho.CpuArm64, macho.TypeExec, payload('b', 10)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Limits fail the header, the padding and the slice copy in turn.
	for _, limit := range []int{0, fatHeaderSize + 2*fatArchSize, sliceAlign} {
		if err := writeFat(&limitWriter{limit: limit}, slices); err == nil {
			t.Errorf("limit %d: writeFat succeeded, want an error", limit)
		}
	}
}

func TestGivenBadArgumentsWhenMainRunsThenExitsOneWithUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"-o", "out"}, {"-o", "out", "one"}, {"-x"}} {
		var stderr bytes.Buffer
		if code := realMain(args, &stderr); code != 1 || !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("args %v: exit %d stderr %q, want exit 1 with usage", args, code, stderr.String())
		}
	}
}
