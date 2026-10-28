package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// tinyGGUF writes a GGUF v3 file with one 16-byte tensor.
func tinyGGUF(t *testing.T, path string) []byte {
	var w ggufWriter
	w.WriteString(ggufMagic)
	_ = binary.Write(&w, binary.LittleEndian, uint32(3))
	_ = binary.Write(&w, binary.LittleEndian, uint64(1)) // tensors
	_ = binary.Write(&w, binary.LittleEndian, uint64(2)) // kvs
	w.kvString("general.architecture", "qwen35")
	w.kvStrings("tokenizer.chat_templates", []string{"tool_use"})
	w.str("t")
	_ = binary.Write(&w, binary.LittleEndian, uint32(1))
	_ = binary.Write(&w, binary.LittleEndian, uint64(4))
	_ = binary.Write(&w, binary.LittleEndian, uint32(0))
	_ = binary.Write(&w, binary.LittleEndian, uint64(0))
	w.Write(make([]byte, alignUp(int64(w.Len()), 32)-int64(w.Len())))
	data := []byte("0123456789abcdef")
	w.Write(data)
	if errWrite := os.WriteFile(path, w.Bytes(), 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	return data
}

func TestGraftDecision(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "m.gguf"), filepath.Join(dir, systemoneName("m.gguf"))
	data := tinyGGUF(t, src)
	before, errRead := readGGUF(src)
	if errRead != nil || before.decisionReady() {
		t.Fatalf("source: %v ready=%v", errRead, before.decisionReady())
	}
	var copied int64
	if errGraft := graftDecision(src, dst, true, func(n int64) { copied += n }); errGraft != nil {
		t.Fatal(errGraft)
	}
	after, errAfter := readGGUF(dst)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if !after.decisionReady() || !after.Keys["qwen35.attention.causal"] || after.Arch != "qwen35" {
		t.Fatalf("missing decision metadata: %v", after.kvKeys)
	}
	if len(after.Templates) != 2 || after.Templates[0] != "systemone" || after.Templates[1] != "tool_use" {
		t.Fatalf("templates = %v", after.Templates)
	}
	raw, _ := os.ReadFile(dst)
	if after.dataStart%32 != 0 || !bytes.Equal(raw[after.dataStart:], data) || copied != int64(len(data)) {
		t.Fatalf("tensor data moved: start %d copied %d", after.dataStart, copied)
	}
	// Grafting again replaces the keys instead of adding them twice; an existing causal key is kept.
	again := filepath.Join(dir, "again.gguf")
	if errGraft := graftDecision(dst, again, false, nil); errGraft != nil {
		t.Fatal(errGraft)
	}
	if m, _ := readGGUF(again); len(m.kvKeys) != len(after.kvKeys) {
		t.Fatalf("keys %d after regraft, want %d", len(m.kvKeys), len(after.kvKeys))
	}
}

func TestHostPath(t *testing.T) {
	env := dockerEnv{InContainer: true, Mounts: []mountPoint{
		{Source: "/srv/cliproxy/plugins", Destination: "/CLIProxyAPI/plugins"},
		{Source: "/data/models", Destination: "/CLIProxyAPI/plugins/data/models"},
	}}
	for in, want := range map[string]string{
		"/CLIProxyAPI/plugins/data/models/a/b": "/data/models/a/b",
		"/CLIProxyAPI/plugins/x":               "/srv/cliproxy/plugins/x",
		"/CLIProxyAPI/pluginsX":                "",
	} {
		got, errHost := hostPath(env, in)
		if got != want || (want == "") != (errHost != nil) {
			t.Errorf("hostPath(%s) = %q, %v; want %q", in, got, errHost, want)
		}
	}
}

func TestDemuxLogs(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		h := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
		return append(h, s...)
	}
	raw := append(frame(1, "hello\n"), frame(2, "world\n")...)
	if got := demuxLogs(raw); got != "hello\nworld\n" {
		t.Fatalf("demux = %q", got)
	}
	if got := demuxLogs([]byte("plain tty output")); got != "plain tty output" {
		t.Fatalf("tty = %q", got)
	}
}
