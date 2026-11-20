// Command mmc-jit-files decodes one GitHub JIT bundle into the private
// RAM-backed runner root prepared by the image entrypoint.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := unpack(os.Stdin); err != nil {
		// Decoder and filesystem errors can include untrusted input.
		fmt.Fprintln(os.Stderr, "could not prepare runner configuration")
		os.Exit(1)
	}
}

func unpack(in io.Reader) error {
	const limit = 1 << 20
	encoded, err := bufio.NewReader(io.LimitReader(in, limit+1)).ReadBytes('\n')
	if (err != nil && err != io.EOF) || len(encoded) > limit {
		return errors.New("invalid JIT input")
	}
	var files map[string][]byte
	decoder := json.NewDecoder(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(bytes.TrimSpace(encoded))))
	if err := decoder.Decode(&files); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF || len(files[".runner"]) == 0 || len(files[".credentials"]) == 0 {
		return errors.New("invalid JIT bundle")
	}
	for name, data := range files {
		switch name {
		case ".runner", ".credentials", ".credentials_rsaparams", ".runner_migrated", ".credentials_migrated":
		default:
			return errors.New("unexpected JIT file")
		}
		if len(data) == 0 {
			return errors.New("empty JIT file")
		}
	}
	for name, data := range files {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(data)
		if err := errors.Join(writeErr, f.Close()); err != nil {
			return err
		}
	}
	return nil
}
