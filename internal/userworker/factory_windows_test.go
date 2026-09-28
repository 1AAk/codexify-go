//go:build windows

package userworker

import (
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReadEnvironmentBlock(t *testing.T) {
	entries := []string{"A=1", "Path=C:\\Tools", "=C:=C:\\Work"}
	var block []uint16
	for _, entry := range entries {
		u, err := windows.UTF16FromString(entry)
		if err != nil {
			t.Fatal(err)
		}
		block = append(block, u...)
	}
	block = append(block, 0)

	got := readEnvironmentBlock(&block[0])
	if !reflect.DeepEqual(got, entries) {
		t.Fatalf("got %#v want %#v", got, entries)
	}
}
