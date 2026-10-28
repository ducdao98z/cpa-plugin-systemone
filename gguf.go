package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// decisionTemplate is the prompt llama.cpp's /v1/systemone renders, in ChatML as used by Qwen-based
// decision models such as elephant-1. It is added to GGUF files that have no systemone template.
//
//go:embed decision.jinja
var decisionTemplate string

const (
	ggufMagic      = "GGUF"
	ggufTypeUint32 = 4
	ggufTypeF32    = 6
	ggufTypeBool   = 7
	ggufTypeString = 8
	ggufTypeArray  = 9
)

// ggufMeta is what the plugin needs from a GGUF header.
type ggufMeta struct {
	Arch      string
	Alignment uint64
	Keys      map[string]bool
	Templates []string // names in tokenizer.chat_templates

	version    uint32
	tensors    uint64
	kvs        [][]byte // raw key/value records, in file order
	kvKeys     []string
	tensorInfo []byte // raw tensor info records
	dataStart  int64  // file offset of the tensor data
}

// decisionReady says whether llama.cpp can serve /v1/systemone from this file as it is.
func (m ggufMeta) decisionReady() bool {
	return m.Keys[m.Arch+".decision.type"] && m.Keys["tokenizer.chat_template.systemone"]
}

type ggufReader struct {
	r   *bufio.Reader
	buf bytes.Buffer // bytes of the record being read
	n   int64
}

func (g *ggufReader) read(p []byte) error {
	if _, errRead := io.ReadFull(g.r, p); errRead != nil {
		return errRead
	}
	g.buf.Write(p)
	g.n += int64(len(p))
	return nil
}

func (g *ggufReader) u32() (uint32, error) {
	var b [4]byte
	errRead := g.read(b[:])
	return binary.LittleEndian.Uint32(b[:]), errRead
}

func (g *ggufReader) u64() (uint64, error) {
	var b [8]byte
	errRead := g.read(b[:])
	return binary.LittleEndian.Uint64(b[:]), errRead
}

func (g *ggufReader) str() (string, error) {
	n, errLen := g.u64()
	if errLen != nil {
		return "", errLen
	}
	if n > 64<<20 {
		return "", errors.New("gguf: string too long")
	}
	b := make([]byte, n)
	errRead := g.read(b)
	return string(b), errRead
}

var ggufScalarSize = map[uint32]int{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

// value reads one value of type t. Strings and uint32 values are returned, others are skipped.
func (g *ggufReader) value(t uint32) (any, error) {
	switch t {
	case ggufTypeString:
		return g.str()
	case ggufTypeArray:
		itemType, errType := g.u32()
		if errType != nil {
			return nil, errType
		}
		n, errLen := g.u64()
		if errLen != nil {
			return nil, errLen
		}
		var items []string
		for i := uint64(0); i < n; i++ {
			v, errItem := g.value(itemType)
			if errItem != nil {
				return nil, errItem
			}
			if s, ok := v.(string); ok && itemType == ggufTypeString {
				items = append(items, s)
			}
		}
		return items, nil
	}
	size, ok := ggufScalarSize[t]
	if !ok {
		return nil, fmt.Errorf("gguf: unknown value type %d", t)
	}
	b := make([]byte, size)
	if errRead := g.read(b); errRead != nil {
		return nil, errRead
	}
	if t == ggufTypeUint32 {
		return binary.LittleEndian.Uint32(b), nil
	}
	return nil, nil
}

// readGGUF parses the header, metadata and tensor infos of a GGUF v2/v3 file.
func readGGUF(path string) (ggufMeta, error) {
	f, errOpen := os.Open(path)
	if errOpen != nil {
		return ggufMeta{}, errOpen
	}
	defer func() { _ = f.Close() }()
	g := &ggufReader{r: bufio.NewReaderSize(f, 1<<20)}
	m := ggufMeta{Alignment: 32, Keys: map[string]bool{}}
	magic := make([]byte, 4)
	if errMagic := g.read(magic); errMagic != nil || string(magic) != ggufMagic {
		return m, errors.New("not a GGUF file")
	}
	var errHead error
	if m.version, errHead = g.u32(); errHead != nil || m.version < 2 {
		return m, fmt.Errorf("unsupported GGUF version %d", m.version)
	}
	if m.tensors, errHead = g.u64(); errHead != nil {
		return m, errHead
	}
	kvCount, errKV := g.u64()
	if errKV != nil {
		return m, errKV
	}
	for i := uint64(0); i < kvCount; i++ {
		g.buf.Reset()
		key, errKey := g.str()
		if errKey != nil {
			return m, errKey
		}
		t, errType := g.u32()
		if errType != nil {
			return m, errType
		}
		v, errValue := g.value(t)
		if errValue != nil {
			return m, fmt.Errorf("gguf: %s: %w", key, errValue)
		}
		switch key {
		case "general.architecture":
			m.Arch, _ = v.(string)
		case "general.alignment":
			if a, ok := v.(uint32); ok && a > 0 {
				m.Alignment = uint64(a)
			}
		case "tokenizer.chat_templates":
			m.Templates, _ = v.([]string)
		}
		m.Keys[key] = true
		m.kvKeys = append(m.kvKeys, key)
		m.kvs = append(m.kvs, bytes.Clone(g.buf.Bytes()))
	}
	g.buf.Reset()
	for i := uint64(0); i < m.tensors; i++ {
		if _, errName := g.str(); errName != nil {
			return m, errName
		}
		dims, errDims := g.u32()
		if errDims != nil {
			return m, errDims
		}
		if errInfo := g.read(make([]byte, 8*int(dims)+4+8)); errInfo != nil { // dims, type, offset
			return m, errInfo
		}
	}
	m.tensorInfo = bytes.Clone(g.buf.Bytes())
	m.dataStart = alignUp(g.n, m.Alignment)
	return m, nil
}

func alignUp(n int64, a uint64) int64 {
	return (n + int64(a) - 1) / int64(a) * int64(a)
}

type ggufWriter struct{ bytes.Buffer }

func (w *ggufWriter) str(s string) {
	_ = binary.Write(w, binary.LittleEndian, uint64(len(s)))
	w.WriteString(s)
}

func (w *ggufWriter) kvString(key, value string) {
	w.str(key)
	_ = binary.Write(w, binary.LittleEndian, uint32(ggufTypeString))
	w.str(value)
}

func (w *ggufWriter) kvF32(key string, value float32) {
	w.str(key)
	_ = binary.Write(w, binary.LittleEndian, uint32(ggufTypeF32))
	_ = binary.Write(w, binary.LittleEndian, math.Float32bits(value))
}

func (w *ggufWriter) kvStrings(key string, values []string) {
	w.str(key)
	_ = binary.Write(w, binary.LittleEndian, uint32(ggufTypeArray))
	_ = binary.Write(w, binary.LittleEndian, uint32(ggufTypeString))
	_ = binary.Write(w, binary.LittleEndian, uint64(len(values)))
	for _, v := range values {
		w.str(v)
	}
}

// graftDecision writes dst as a copy of src with the metadata llama.cpp needs to answer /v1/systemone:
// a chat template named systemone and the openjev decision readout (label-token logits). noncausal marks
// models trained with bidirectional attention inside prompt blocks. Weights are copied byte for byte.
// progress receives the bytes of tensor data copied so far.
func graftDecision(src, dst string, noncausal bool, progress func(int64)) error {
	m, errRead := readGGUF(src)
	if errRead != nil {
		return errRead
	}
	if m.Arch == "" {
		return errors.New("the GGUF file has no general.architecture")
	}
	added := map[string]bool{
		"tokenizer.chat_template.systemone": true, "tokenizer.chat_templates": true, m.Arch + ".decision.type": true,
	}
	qtypes := []string{"choice", "score", "noul"}
	for _, q := range qtypes {
		added[m.Arch+".decision.temperature."+q] = true
	}
	causalKey := m.Arch + ".attention.causal"
	if noncausal {
		added[causalKey] = true
	}
	templates := []string{"systemone"}
	for _, name := range m.Templates {
		if name != "systemone" {
			templates = append(templates, name)
		}
	}

	var w ggufWriter
	w.WriteString(ggufMagic)
	_ = binary.Write(&w, binary.LittleEndian, m.version)
	_ = binary.Write(&w, binary.LittleEndian, m.tensors)
	kept := 0
	for _, k := range m.kvKeys {
		if !added[k] {
			kept++
		}
	}
	_ = binary.Write(&w, binary.LittleEndian, uint64(kept+len(added)))
	for i, k := range m.kvKeys {
		if !added[k] {
			w.Write(m.kvs[i])
		}
	}
	w.kvString("tokenizer.chat_template.systemone", decisionTemplate)
	w.kvStrings("tokenizer.chat_templates", templates)
	w.kvString(m.Arch+".decision.type", "openjev")
	for _, q := range qtypes {
		w.kvF32(m.Arch+".decision.temperature."+q, 1.0)
	}
	if noncausal {
		w.str(causalKey)
		_ = binary.Write(&w, binary.LittleEndian, uint32(ggufTypeBool))
		w.WriteByte(0)
	}
	w.Write(m.tensorInfo)
	w.Write(make([]byte, alignUp(int64(w.Len()), m.Alignment)-int64(w.Len())))

	in, errOpen := os.Open(src)
	if errOpen != nil {
		return errOpen
	}
	defer func() { _ = in.Close() }()
	if _, errSeek := in.Seek(m.dataStart, io.SeekStart); errSeek != nil {
		return errSeek
	}
	part := dst + partSuffix
	out, errCreate := os.Create(part)
	if errCreate != nil {
		return errCreate
	}
	_, errCopy := out.Write(w.Bytes())
	if errCopy == nil {
		_, errCopy = io.Copy(countWriter{out, progress}, in)
	}
	if errClose := out.Close(); errCopy == nil {
		errCopy = errClose
	}
	if errCopy != nil {
		_ = os.Remove(part)
		return errCopy
	}
	return os.Rename(part, dst)
}

type countWriter struct {
	w    io.Writer
	done func(int64)
}

func (c countWriter) Write(b []byte) (int, error) {
	n, errWrite := c.w.Write(b)
	if c.done != nil {
		c.done(int64(n))
	}
	return n, errWrite
}

// systemoneName is where the grafted copy of a GGUF file goes.
func systemoneName(file string) string {
	return strings.TrimSuffix(file, ".gguf") + ".systemone.gguf"
}
