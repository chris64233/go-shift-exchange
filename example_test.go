package goshiftexchange_test

import (
	"fmt"
	"time"

	gx "github.com/chris64233/go-shift-exchange"
)

// ExampleService_atomicSwap 演示一次两人原子换班：收齐同意前排班不变，
// 最后一次同意在同一临界区内完成版本核对、整体复核与一次性落班。
func ExampleService_atomicSwap() {
	svc, err := gx.NewService(gx.NewMemoryStore())
	if err != nil {
		panic(err)
	}
	if _, err := svc.RegisterEmployee("alice", "Alice", []string{"A"}); err != nil {
		panic(err)
	}
	if _, err := svc.RegisterEmployee("carol", "Carol", []string{"A"}); err != nil {
		panic(err)
	}

	at := func(day int, h, m int) time.Time {
		return time.Date(2026, 10, day, h, m, 0, 0, time.UTC)
	}
	mustShift := func(id, emp string, start, end time.Time) {
		if _, err := svc.ScheduleShift(gx.Shift{
			ID: id, Position: "A", EmployeeID: emp, Start: start, End: end,
		}); err != nil {
			panic(err)
		}
	}
	mustShift("s1", "alice", at(1, 8, 0), at(1, 16, 0))
	mustShift("s3", "carol", at(3, 8, 0), at(3, 16, 0))

	// 申请：alice 与 carol 互换各自的班次；提交时冻结 s1、s3 的 v1 快照。
	req, err := svc.CreateSwapRequest([]gx.Rotation{
		{ShiftID: "s1", NewEmployeeID: "carol"},
		{ShiftID: "s3", NewEmployeeID: "alice"},
	})
	if err != nil {
		panic(err)
	}

	if _, err := svc.Agree(req.ID, "alice"); err != nil { // 只一人同意
		panic(err)
	}
	if sh, _ := svc.GetShift("s1"); sh.EmployeeID != "alice" {
		panic("schedule changed before all participants agreed")
	}

	if _, err := svc.Agree(req.ID, "carol"); err != nil { // 收齐，原子生效
		panic(err)
	}
	got, _ := svc.GetSwapRequest(req.ID)
	fmt.Printf("swap status: %s\n", got.Status)
	for _, id := range []string{"s1", "s3"} {
		sh, _ := svc.GetShift(id)
		fmt.Printf("%s: %s (v%d)\n", id, sh.EmployeeID, sh.Version)
	}
	// Output:
	// swap status: completed
	// s1: carol (v2)
	// s3: alice (v2)
}
