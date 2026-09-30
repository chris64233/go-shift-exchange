package goshiftexchange

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// fakeClock 是可手动推进的测试时钟。
type fakeClock struct{ t time.Time }

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }
func (c *fakeClock) now() time.Time           { return c.t }
func (c *fakeClock) advance(d time.Duration)  { c.t = c.t.Add(d) }

func TestListSwapRequests_OrderedByCreation(t *testing.T) {
	clock := newFakeClock(dayTime(1, 0, 0))
	svc, err := NewService(NewMemoryStore(), WithClock(clock.now))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		id  string
		pos []string
	}{
		{"alice", []string{"A"}}, {"carol", []string{"A"}},
	} {
		if _, err := svc.RegisterEmployee(e.id, e.id, e.pos); err != nil {
			t.Fatal(err)
		}
	}
	mkShift := func(id, emp string, day int) {
		if _, err := svc.ScheduleShift(Shift{
			ID: id, Position: "A", EmployeeID: emp,
			Start: dayTime(day, 8, 0), End: dayTime(day, 16, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkShift("s1", "alice", 1)
	mkShift("s3", "carol", 3)
	// 第一份申请随即拒绝，第二份之后创建，列表必须按创建时间而非状态排序。
	r1, err := svc.CreateSwapRequest([]Rotation{{"s1", "carol"}, {"s3", "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reject(r1.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	r2, err := svc.CreateSwapRequest([]Rotation{{"s1", "carol"}, {"s3", "alice"}})
	if err != nil {
		t.Fatal(err)
	}

	list := svc.ListSwapRequests()
	if len(list) != 2 || list[0].ID != r1.ID || list[1].ID != r2.ID {
		t.Fatalf("order = [%s %s], want [%s %s]",
			list[0].ID, list[1].ID, r1.ID, r2.ID)
	}
	if _, err := svc.GetSwapRequest("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSwapRequest: want ErrNotFound, got %v", err)
	}
}

func TestReject_DuplicateIdempotent(t *testing.T) {
	f := newFixture(t)
	req, _ := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	first, err := f.svc.Reject(req.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if first.Idempotent || first.Request.Status != SwapRejected {
		t.Fatalf("first reject = %+v", first)
	}
	second, err := f.svc.Reject(req.ID, "alice")
	if err != nil {
		t.Fatalf("duplicate reject: %v", err)
	}
	if !second.Idempotent || second.Request.Status != SwapRejected {
		t.Fatalf("duplicate reject = %+v, want idempotent rejected", second)
	}
	// 另一个人在终态上拒绝仍是非幂等错误。
	if _, err := f.svc.Reject(req.ID, "carol"); err == nil {
		t.Fatal("reject by other participant on terminal request must fail")
	}
}

func TestScheduleShift_InvalidShapeAndUnknownEmployee(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.ScheduleShift(Shift{ID: " "}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank id: want ErrInvalidInput, got %v", err)
	}
	// 缺少起止时间的班次没有意义，必须拒绝。
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "x", Position: "A", EmployeeID: "alice",
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero time range: want ErrInvalidInput, got %v", err)
	}
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "x", Position: "A", EmployeeID: "alice",
		End: dayTime(1, 16, 0),
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing start: want ErrInvalidInput, got %v", err)
	}
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "x", Position: "A", EmployeeID: "alice",
		Start: dayTime(1, 10, 0), End: dayTime(1, 9, 0),
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("end before start: want ErrInvalidInput, got %v", err)
	}
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "x", Position: "A", EmployeeID: "ghost",
		Start: dayTime(1, 8, 0), End: dayTime(1, 16, 0),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown employee: want ErrNotFound, got %v", err)
	}
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "s1", Position: "A", EmployeeID: "alice",
		Start: dayTime(9, 8, 0), End: dayTime(9, 16, 0),
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate shift: want ErrAlreadyExists, got %v", err)
	}
}

// TestScheduleShift_EndpointTouching 验证端点相接不算时间重叠：
// 关闭最短休息约束时，前班结束即后班开始是合法的；
// 默认 8h 休息约束下同一排布则因休息不足被拒。
func TestScheduleShift_EndpointTouching(t *testing.T) {
	svc, err := NewService(NewMemoryStore(), WithMinRest(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterEmployee("alice", "Alice", []string{"A"}); err != nil {
		t.Fatal(err)
	}
	backToBack := []Shift{
		{ID: "e1", Position: "A", EmployeeID: "alice",
			Start: dayTime(1, 8, 0), End: dayTime(1, 16, 0)},
		{ID: "e2", Position: "A", EmployeeID: "alice",
			Start: dayTime(1, 16, 0), End: dayTime(1, 20, 0)},
	}
	for _, sh := range backToBack {
		if _, err := svc.ScheduleShift(sh); err != nil {
			t.Fatalf("endpoint-touching with minRest=0 should be legal: %v", err)
		}
	}

	// 同一排布在默认 8h 休息约束下必须因休息不足被拒。
	f := newFixture(t)
	if _, err := f.svc.ScheduleShift(Shift{
		ID: "tight", Position: "A", EmployeeID: "alice",
		Start: dayTime(1, 16, 0), End: dayTime(1, 20, 0),
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("back-to-back under 8h rest: want ErrInvalidInput, got %v", err)
	}

	// overlaps 单元语义：端点相接不重叠，任一方向相交则重叠。
	if overlaps(dayTime(1, 8, 0), dayTime(1, 16, 0), dayTime(1, 16, 0), dayTime(1, 20, 0)) {
		t.Fatal("endpoint-touching intervals must not overlap")
	}
	if !overlaps(dayTime(1, 8, 0), dayTime(1, 16, 0), dayTime(1, 15, 0), dayTime(1, 20, 0)) {
		t.Fatal("intersecting intervals must overlap")
	}
}

func TestFileStore_BadPath(t *testing.T) {
	// 父目录不存在且无法创建。
	if _, err := NewFileStore(filepath.Join(t.TempDir(), "no-such-dir", "e.jsonl")); err == nil {
		t.Fatal("opening an impossible path should fail")
	}
}
