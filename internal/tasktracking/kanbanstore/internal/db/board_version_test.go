package db

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kayushkin/kanban-store/internal/model"
)

func TestBoardVersionConcurrentWriters(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "boards.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	description := "Must survive"
	board, err := store.CreateBoard(&model.CreateBoardRequest{Name: "Original", Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			name := "Updated"
			_, err := store.UpdateBoardIfVersion(board.ID, &model.UpdateBoardRequest{Name: &name}, &board.UpdatedAt)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrBoardChanged) {
			t.Fatalf("unexpected save failure: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful writers = %d, want 1", successes)
	}
	got, err := store.GetBoard(board.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != board.Description || !got.UpdatedAt.After(board.UpdatedAt) {
		t.Fatalf("unexpected saved board: %+v", got)
	}
	name := "Legacy update"
	legacy, err := store.UpdateBoard(board.ID, &model.UpdateBoardRequest{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.UpdatedAt.After(got.UpdatedAt) {
		t.Fatal("version did not advance")
	}
	if _, err := store.UpdateBoardIfVersion(board.ID, &model.UpdateBoardRequest{Name: &name}, &got.UpdatedAt); !errors.Is(err, ErrBoardChanged) {
		t.Fatalf("old version accepted after unconditional save: %v", err)
	}
}
