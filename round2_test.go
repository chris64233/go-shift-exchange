package goshiftexchange

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScheduleShift_RequiresStartAndEnd 班次必须给出完整时间区间：
// 零值时间无法参与“不重叠 / 最短休息”校验，必须在排班入口拦截。
func TestScheduleShift_RequiresStartAndEnd(t *testing.T) {
	f := newFixture(t)

	missingTimes := []Shift{
		{ID: "z1", Position: "A", EmployeeID: "alice"},
		{ID: "z2", Position: "A", EmployeeID: "alice", Start: dayTime(10, 8, 0)},
		{ID: "z3", Position: "A", EmployeeID: "alice", End: dayTime(10, 16, 0)},
	}
	for _, sh := range missingTimes {
		if _, err := f.svc.ScheduleShift(sh); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("shift %s with missing times: want ErrInvalidInput, got %v", sh.ID, err)
		}
	}
}

// TestCreateSwapRequest_RejectsNonCycleRedistribution 每人必须交出一个班次、
// 恰好接一个班次：2 名员工之间重分配 4 个班次（有人接两个/交两个）不是循环
// 轮换，必须拒绝，即使每个目标新持有人都是当前持有人之一。
func TestCreateSwapRequest_RejectsNonCycleRedistribution(t *testing.T) {
	svc, _ := NewService(NewMemoryStore(), WithMinRest(0))
	for _, e := range []struct{ id, pos string }{
		{"alice", "A"}, {"carol", "A"},
	} {
		if _, err := svc.RegisterEmployee(e.id, e.id, []string{e.pos}); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(id, emp string, day int) {
		t.Helper()
		if _, err := svc.ScheduleShift(Shift{
			ID: id, Position: "A", EmployeeID: emp,
			Start: dayTime(day, 8, 0), End: dayTime(day, 16, 0),
		}); err != nil {
			t.Fatalf("schedule %s: %v", id, err)
		}
	}
	mk("a1", "alice", 1)
	mk("a2", "alice", 2)
	mk("c1", "carol", 3)
	mk("c2", "carol", 4)

	// alice 交出 a1/a2、carol 交出 c1/c2，新持有人计数 2=2、彼此都是持有人，
	// 没有“条数 == 人数”这一条就会被错误放行（有人交两个、接两个，并非循环）。
	_, err := svc.CreateSwapRequest([]Rotation{
		{"a1", "carol"}, {"a2", "carol"},
		{"c1", "alice"}, {"c2", "alice"},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("4-shift redistribution among 2 people must be rejected, got %v", err)
	}

	// 同样的 2 人 2 班次真正循环仍然合法。
	req, err := svc.CreateSwapRequest([]Rotation{
		{"a1", "carol"}, {"c1", "alice"},
	})
	if err != nil {
		t.Fatalf("plain 2-way swap should be legal: %v", err)
	}
	if len(req.Participants) != 2 {
		t.Fatalf("participants = %v, want 2", req.Participants)
	}
}

// TestAgree_VersionConflict_HolderChainChanged 覆盖“同一班次先加入别的换班申请
// 并被改动”的链路：旧申请挂起期间班次版本先被另一份申请推进，旧申请收齐同意
// 时必须失败，且不得覆盖已经生效的新排班（版本号与持有人都保持为新安排）。
func TestAgree_VersionConflict_HolderChainChanged(t *testing.T) {
	f := newFixture(t, WithMinRest(0))
	mk := func(id, emp string, day int) {
		t.Helper()
		if _, err := f.svc.ScheduleShift(Shift{
			ID: id, Position: "A", EmployeeID: emp,
			Start: dayTime(day, 8, 0), End: dayTime(day, 16, 0),
		}); err != nil {
			t.Fatalf("schedule %s: %v", id, err)
		}
	}
	mk("a2", "alice", 10)
	mk("c4", "carol", 11)

	// 旧申请涉及 a2、c4，先收一票挂起。
	oldReq, err := f.svc.CreateSwapRequest([]Rotation{
		{"a2", "carol"}, {"c4", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Agree(oldReq.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	// 另一份申请先完成，使 a2/c4 持有人翻转、版本 +1。
	midReq, err := f.svc.CreateSwapRequest([]Rotation{
		{"a2", "carol"}, {"c4", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.agreeAll(t, midReq.ID, "alice", "carol")
	if h, v := f.holder(t, "a2"); h != "carol" || v != 2 {
		t.Fatalf("a2 after mid swap = %s v%d", h, v)
	}

	// 旧申请收齐：必须失败，不能再动已被新安排覆盖的班次。
	res, err := f.svc.Agree(oldReq.ID, "carol")
	if err != nil {
		t.Fatalf("Agree: %v", err)
	}
	if res.Request.Status != SwapFailed || res.Request.FailReason == "" {
		t.Fatalf("old request = %s reason=%q, want failed with reason",
			res.Request.Status, res.Request.FailReason)
	}
	if h, v := f.holder(t, "a2"); h != "carol" || v != 2 {
		t.Fatalf("a2 = %s v%d, newer arrangement must be preserved", h, v)
	}
	if h, v := f.holder(t, "c4"); h != "alice" || v != 2 {
		t.Fatalf("c4 = %s v%d, newer arrangement must be preserved", h, v)
	}
}

// TestHistory_PreservesSnapshotAndOutcome 快照与最终结果随记录持久化：
// 完成的申请能查到最终改派；拒绝的申请保留冻结快照但不产生任何变更。
func TestHistory_PreservesSnapshotAndOutcome(t *testing.T) {
	f := newFixture(t)

	// 第一份：完成。alice 与 carol 互换 s1/s3。
	done, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.agreeAll(t, done.ID, "alice", "carol")

	// 第二份：被拒绝。快照仍应保留在 alice 的历史里。
	rejected, err := f.svc.CreateSwapRequest([]Rotation{
		{"s1", "alice"}, {"s3", "carol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Reject(rejected.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	h, err := f.svc.HistoryForEmployee("alice")
	if err != nil {
		t.Fatal(err)
	}
	byStatus := map[SwapStatus]*SwapRequest{}
	for i := range h.SwapRequests {
		r := h.SwapRequests[i]
		byStatus[r.Status] = &r
	}
	comp, ok1 := byStatus[SwapCompleted]
	rej, ok2 := byStatus[SwapRejected]
	if !ok1 || !ok2 {
		t.Fatalf("history requests = %+v, want one completed and one rejected", h.SwapRequests)
	}

	// 完成申请：快照冻结的是 v1，最终变更记录版本 v1 -> v2。
	if len(comp.Snapshots) != 2 {
		t.Fatalf("completed snapshots = %d, want 2", len(comp.Snapshots))
	}
	for _, snap := range comp.Snapshots {
		if snap.Shift.Version != 1 {
			t.Fatalf("snapshot %s version = %d, want frozen v1", snap.Shift.ID, snap.Shift.Version)
		}
	}
	if comp.Finalization == nil || !comp.Finalization.Completed || len(comp.Finalization.Changes) != 2 {
		t.Fatalf("completed finalization = %+v", comp.Finalization)
	}
	for _, ch := range comp.Finalization.Changes {
		if ch.NewVersion != ch.OldVersion+1 {
			t.Fatalf("change versions not consecutive: %+v", ch)
		}
	}

	// 拒绝申请：快照保留、没有最终变更，排班不变。
	if len(rej.Snapshots) != 2 {
		t.Fatalf("rejected snapshots = %d, want 2 (snapshot must persist)", len(rej.Snapshots))
	}
	if rej.Finalization != nil {
		t.Fatalf("rejected request must not carry finalization: %+v", rej.Finalization)
	}
	if h, _ := f.holder(t, "s1"); h != "carol" {
		t.Fatalf("s1 = %s, rejected swap must not change schedule", h)
	}
	if len(h.Changes) != 2 {
		t.Fatalf("history changes = %d, want exactly the 2 completed changes", len(h.Changes))
	}
	if changes := f.svc.ListChanges(); len(changes) != 2 {
		t.Fatalf("global changes = %d, want 2", len(changes))
	}
}

// TestFileStore_RecoversTornTail 模拟追加到一半掉电：文件末尾留下不带换行的
// 半行 JSON。重新打开必须截断半行、保留此前完整事件，重放与继续追加都正常。
func TestFileStore_RecoversTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterEmployee("alice", "Alice", []string{"A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterEmployee("carol", "Carol", []string{"A"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 直接在文件尾部追加一段不完整、且没有换行结尾的半行。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":3,"type":"employee_`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：半行被截断，之前两名员工完好。
	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen with torn tail: %v", err)
	}
	svc2, err := NewService(store2)
	if err != nil {
		t.Fatalf("replay with torn tail: %v", err)
	}
	if emps := svc2.ListEmployees(); len(emps) != 2 {
		t.Fatalf("employees after recovery = %d, want 2", len(emps))
	}

	// 恢复后继续追加：新事件自成一行，Seq 接在历史之后。
	if _, err := svc2.RegisterEmployee("bob", "Bob", nil); err != nil {
		t.Fatalf("append after recovery: %v", err)
	}
	if emps := svc2.ListEmployees(); len(emps) != 3 {
		t.Fatalf("employees after new append = %d, want 3", len(emps))
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	// 落盘文件必须每行都是完整 JSON，且共 3 行（半行被丢弃）。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitJSONLines(data)
	if len(lines) != 3 {
		t.Fatalf("event lines after recovery = %d, want 3; data=%q", len(lines), data)
	}
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("invalid JSON line after recovery: %q", line)
		}
	}
}

// splitJSONLines 按换行切分，忽略末尾空行。
func splitJSONLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			out = append(out, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

// TestListEmployeesAndShifts_Ordered 列表接口返回稳定顺序：
// 员工按 ID、班次按开始时间再按 ID。
func TestListEmployeesAndShifts_Ordered(t *testing.T) {
	svc, _ := NewService(NewMemoryStore())
	for _, e := range []struct{ id, pos string }{
		{"carol", "A"}, {"alice", "A"}, {"bob", "A"},
	} {
		if _, err := svc.RegisterEmployee(e.id, e.id, []string{e.pos}); err != nil {
			t.Fatal(err)
		}
	}
	if got := svc.ListEmployees(); len(got) != 3 ||
		got[0].ID != "alice" || got[1].ID != "bob" || got[2].ID != "carol" {
		var ids []string
		for _, e := range got {
			ids = append(ids, e.ID)
		}
		t.Fatalf("employee order = %v, want [alice bob carol]", ids)
	}

	// alice 只有 A 资质，班次间隔 >=15h，排四个互不冲突的班。
	mk := func(id string, day, hour int) {
		t.Helper()
		if _, err := svc.ScheduleShift(Shift{
			ID: id, Position: "A", EmployeeID: "alice",
			Start: dayTime(day, hour, 0), End: dayTime(day, hour+1, 0),
		}); err != nil {
			t.Fatalf("schedule %s: %v", id, err)
		}
	}
	mk("late", 3, 8)
	mk("early", 1, 8)
	mk("sameA", 2, 8)
	mk("sameB", 2, 17) // 与 sameA(08-09) 恰好间隔 8h，合法且仍排在同一天
	got := svc.ListShifts()
	want := []string{"early", "sameA", "sameB", "late"}
	if len(got) != len(want) {
		t.Fatalf("shift count = %d, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("shift order[%d] = %s, want %s", i, got[i].ID, id)
		}
	}
}

// TestFileStore_RecoversTornTail_AcrossChunkBoundary 当完整事件本身大于
// 64KB 恢复窗口时，分块反向扫描仍必须只截断末尾半行、保留全部完整事件。
func TestFileStore_RecoversTornTail_AcrossChunkBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	// 每条事件超过 64KB（name 很长），制造跨多个恢复窗口的日志。
	longName := strings.Repeat("x", 70*1024)
	const bigEmployees = 3
	for i := 0; i < bigEmployees; i++ {
		id := "big" + string(rune('a'+i))
		if _, err := svc.RegisterEmployee(id, longName, nil); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 追加一个跨边界的大事件的“半行”（无换行结尾）。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	torn := `{"seq":99,"type":"employee_registered","payload":{"employee":{"id":"half"`
	if _, err := f.WriteString(torn); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	svc2, err := NewService(store2)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := len(svc2.ListEmployees()); got != bigEmployees {
		t.Fatalf("employees after cross-boundary recovery = %d, want %d", got, bigEmployees)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitJSONLines(data)
	if len(lines) != bigEmployees {
		t.Fatalf("lines after cross-boundary recovery = %d, want %d", len(lines), bigEmployees)
	}
	for i, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("line %d is invalid JSON", i)
		}
	}
}
