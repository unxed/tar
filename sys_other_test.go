//go:build !linux && !freebsd && !darwin && !windows
// +build !linux,!freebsd,!darwin,!windows

package tar

import "testing"

func TestSysOther(t *testing.T) {
	// Просто вызываем функции, чтобы покрыть заглушки (stubs) на unsupported платформах.
	sysHeader(nil, nil)

	if err := extractSpecialFile("", nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if err := sysXattrs("", nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if err := applyXattrs("", nil); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}
