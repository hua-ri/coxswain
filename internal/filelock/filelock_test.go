package filelock

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestWithSerialises(t *testing.T) {
	dir := t.TempDir()
	lock, file := filepath.Join(dir, "sub", ".lock"), filepath.Join(dir, "n")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := With(lock, func() error {
				b, _ := os.ReadFile(file)
				n, _ := strconv.Atoi(string(b))
				return WriteAtomic(file, []byte(strconv.Itoa(n+1)))
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if b, _ := os.ReadFile(file); string(b) != "20" {
		t.Fatalf("counter = %s", b)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}
