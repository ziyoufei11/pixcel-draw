package main_test

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestRoom(t *testing.T) {
	ext := filepath.Ext("a.jpg")
	fmt.Println(ext)
}
