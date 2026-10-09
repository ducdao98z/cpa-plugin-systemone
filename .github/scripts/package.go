//go:build ignore

// package.go zips one plugin library at the archive root and writes its sha256sum line,
// the layout the CLIProxyAPI plugin store installs from.
//
//	go run .github/scripts/package.go <library> <archive.zip>
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run package.go <library> <archive.zip>")
		os.Exit(2)
	}
	if err := pack(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func pack(library, archive string) error {
	lib, err := os.Open(library)
	if err != nil {
		return err
	}
	defer lib.Close()
	out, err := os.Create(archive)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	header := &zip.FileHeader{Name: filepath.Base(library), Method: zip.Deflate}
	header.SetMode(0o755)
	w, err := zw.CreateHeader(header)
	if err == nil {
		_, err = io.Copy(w, lib)
	}
	if err == nil {
		err = zw.Close()
	}
	if errClose := out.Close(); err == nil {
		err = errClose
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	line := hex.EncodeToString(sum[:]) + "  " + filepath.Base(archive) + "\n"
	return os.WriteFile(archive+".sha256", []byte(line), 0o644)
}
