package db

import (
	"errors"
	"testing"
)

func TestIsBusyErr(t *testing.T) {
	busy := []error{
		errors.New("SQLITE_BUSY: database is locked"),
		errors.New("database is locked (5)"),
		errors.New("database table is locked"),
		errors.New("SQLITE_LOCKED"),
	}
	for _, e := range busy {
		if !IsBusyErr(e) {
			t.Errorf("IsBusyErr(%q) = false, want true", e)
		}
	}
	notBusy := []error{nil, errors.New("no such table"), errors.New("disk I/O error")}
	for _, e := range notBusy {
		if IsBusyErr(e) {
			t.Errorf("IsBusyErr(%v) = true, want false", e)
		}
	}
}
