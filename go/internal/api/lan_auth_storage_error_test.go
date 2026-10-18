package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type lanSessionCodedTestError int

func (e lanSessionCodedTestError) Code() int     { return int(e) }
func (e lanSessionCodedTestError) Error() string { return "private storage detail" }

func TestLANSessionStoreErrorCategory(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"busy", lanSessionCodedTestError(5), "sqlite_busy"},
		{"wrapped busy snapshot", fmt.Errorf("save: %w", lanSessionCodedTestError(517)), "sqlite_busy"},
		{"locked", lanSessionCodedTestError(6), "sqlite_locked"},
		{"deadline", fmt.Errorf("save: %w", context.DeadlineExceeded), "deadline"},
		{"other sqlite", lanSessionCodedTestError(10), "storage_error"},
		{"other storage", errors.New("private storage detail"), "storage_error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := lanSessionStoreErrorCategory(tt.err); got != tt.want {
				t.Fatalf("category = %q, want %q", got, tt.want)
			}
		})
	}
}
