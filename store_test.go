package goshiftexchange

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	svc := func() *Service {
		store, err := OpenStore(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return NewService(store, 8*time.Hour)
	}

	s := svc()
	setupRotation(t, s)
	req, err := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Consent(req.ID, "A"); err != nil {
		t.Fatalf("consent A: %v", err)
	}
	// 最后一个同意尚未给出：重启后仍是 pending，同意记录与快照保留
	s2 := svc()
	reopened, err := s2.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("reopen get request: %v", err)
	}
	if reopened.Status != StatusPending || !reopened.Consents["A"] || reopened.Consents["B"] {
		t.Fatalf("pending state not restored: %+v", reopened)
	}
	if len(reopened.Snapshots) != 2 || reopened.Snapshots[0].Version != 1 {
		t.Fatalf("snapshots not restored: %+v", reopened.Snapshots)
	}
	if _, err := s2.Consent(req.ID, "B"); err != nil {
		t.Fatalf("consent B after restart: %v", err)
	}
	if got := s2.ListShifts("A"); len(got) != 1 || got[0].ID != "s2" || got[0].Version != 2 {
		t.Fatalf("swap did not complete after restart: %+v", got)
	}

	// 再次重启：完成状态、历史事件与班次版本都应保留
	s3 := svc()
	final, err := s3.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("reopen completed: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s", final.Status)
	}
	if evs := s3.Events(req.ID); len(evs) != 4 {
		t.Fatalf("expected 4 events restored, got %d", len(evs))
	}
	if emp, err := s3.GetEmployee("A"); err != nil || emp.Name != "A" {
		t.Fatalf("employee not restored: %+v %v", emp, err)
	}

	// ID 序列在重启后继续递增，不会与已有 ID 冲突
	mustEmployee(t, s3, "E", "nurse")
	sh := mustShift(t, s3, "s9", "E", "nurse", at(9, 8), at(9, 16))
	if sh.Version != 1 {
		t.Fatalf("new shift version: %d", sh.Version)
	}
}

func TestPersistencePreservesStaleFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewService(store, 8*time.Hour)
	mustEmployee(t, s, "A", "nurse")
	mustEmployee(t, s, "B", "nurse")
	mustShift(t, s, "s1", "A", "nurse", at(1, 8), at(1, 16))
	mustShift(t, s, "s2", "B", "nurse", at(2, 8), at(2, 16))
	req, _ := s.CreateSwapRequest([]Move{
		{ShiftID: "s1", ToEmployeeID: "B"},
		{ShiftID: "s2", ToEmployeeID: "A"},
	})

	// 直接让班次版本前进（模拟被别的申请抢先修改）
	store.mu.Lock()
	store.state.Shifts["s2"].Version = 5
	store.mu.Unlock()
	_, _ = s.Consent(req.ID, "A")
	_, err = s.Consent(req.ID, "B")
	if err == nil {
		t.Fatal("expected stale failure")
	}

	store2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2 := NewService(store2, 8*time.Hour)
	r2, err := s2.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if r2.Status != StatusFailed {
		t.Fatalf("failed state not persisted, got %s", r2.Status)
	}
}
