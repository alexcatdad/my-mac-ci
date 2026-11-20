package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func bundle(t *testing.T, files map[string][]byte) *strings.Reader {
	t.Helper()
	raw, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(base64.StdEncoding.EncodeToString(raw) + "\n")
}

func TestRejectUnexpectedFilenameBeforeWriting(t *testing.T) {
	t.Chdir(t.TempDir())
	files := map[string][]byte{".runner": []byte("{}"), ".credentials": []byte("{}"), "../outside": []byte("test")}
	if unpack(bundle(t, files)) == nil {
		t.Fatal("accepted a path outside the runner root")
	}
	entries, err := os.ReadDir(".")
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid input left files behind")
	}
}

func TestUnpack(t *testing.T) {
	t.Chdir(t.TempDir())
	files := map[string][]byte{".runner": []byte("settings"), ".credentials": []byte("synthetic")}
	if err := unpack(bundle(t, files)); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != string(want) {
			t.Fatal("decoded content differs")
		}
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("incorrect permissions")
		}
	}
	if unpack(bundle(t, files)) == nil {
		t.Fatal("overwrote existing config")
	}
}
