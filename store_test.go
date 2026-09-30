package goshiftexchange

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestFileStore_PersistsAndReplays 验证事件落盘后，重新构造的 Service
// 通过重放得到完全一致的状态（员工、班次、申请快照、最终变更）。
func TestFileStore_PersistsAndReplays(t *testing.T) {
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
	if _, err := svc.ScheduleShift(Shift{
		ID: "s1", Position: "A", EmployeeID: "alice",
		Start: dayTime(1, 8, 0), End: dayTime(1, 16, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ScheduleShift(Shift{
		ID: "s3", Position: "A", EmployeeID: "carol",
		Start: dayTime(3, 8, 0), End: dayTime(3, 16, 0),
	}); err != nil {
		t.Fatal(err)
	}
	req, err := svc.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Agree(req.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Agree(req.ID, "carol"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 事件文件每行必须是一个合法 JSON 事件，Seq 单调递增。
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var lineCount int
	var lastSeq int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("event line %d: %v", lineCount+1, err)
		}
		if ev.Seq <= lastSeq {
			t.Fatalf("seq not monotonic: %d after %d", ev.Seq, lastSeq)
		}
		lastSeq = ev.Seq
		lineCount++
	}
	f.Close()
	if lineCount != 7 { // 2 员工 + 2 班次 + 1 创建 + 2 同意
		t.Fatalf("event lines = %d, want 7", lineCount)
	}

	// 用全新的 Service 重放同一文件，状态必须一致。
	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	svc2, err := NewService(store2)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := svc2.GetSwapRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != SwapCompleted {
		t.Fatalf("replayed status = %s", replayed.Status)
	}
	if len(replayed.Snapshots) != 2 || replayed.Snapshots[0].Shift.Version != 1 {
		t.Fatalf("replayed snapshots lost: %+v", replayed.Snapshots)
	}
	if replayed.Finalization == nil || len(replayed.Finalization.Changes) != 2 {
		t.Fatalf("replayed finalization lost: %+v", replayed.Finalization)
	}
	for _, want := range []struct{ id, holder string }{
		{"s1", "carol"}, {"s3", "alice"},
	} {
		sh, err := svc2.GetShift(want.id)
		if err != nil {
			t.Fatal(err)
		}
		if sh.EmployeeID != want.holder || sh.Version != 2 {
			t.Fatalf("replayed %s = %s v%d, want %s v2", want.id, sh.EmployeeID, sh.Version, want.holder)
		}
	}
	if len(svc2.ListChanges()) != 2 {
		t.Fatal("replayed changes missing")
	}
}

// TestFileStore_AppendReopen 验证重开日志后可以继续追加且 Seq 不冲突。
func TestFileStore_AppendReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	store1, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc1, err := NewService(store1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc1.RegisterEmployee("alice", "Alice", nil); err != nil {
		t.Fatal(err)
	}
	store1.Close()

	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc2, err := NewService(store2) // 重放已有事件
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.RegisterEmployee("bob", "Bob", nil); err != nil {
		t.Fatal(err)
	}
	if len(svc2.ListEmployees()) != 2 {
		t.Fatalf("employees after reopen = %d, want 2", len(svc2.ListEmployees()))
	}
	store2.Close()

	// 重开后追加的事件 Seq 必须接在历史 Seq 之后，全程单调递增。
	var seqs []int64
	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(f2)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, ev.Seq)
	}
	f2.Close()
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("seqs after reopen = %v, want [1 2]", seqs)
	}
}

// TestFileStore_ReplayPendingThenComplete 验证 pending 状态的申请（含冻结快照
// 与已收集的同意）在重启重放后完整还原，并能继续推进直至原子生效：
// 生效裁决必须使用重放恢复的快照做版本核对。
func TestFileStore_ReplayPendingThenComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	store1, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc1, err := NewService(store1)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ id string }{{"alice"}, {"carol"}} {
		if _, err := svc1.RegisterEmployee(e.id, e.id, []string{"A"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sh := range []struct{ id, emp string }{{"s1", "alice"}, {"s3", "carol"}} {
		day := 1
		if sh.id == "s3" {
			day = 3
		}
		if _, err := svc1.ScheduleShift(Shift{
			ID: sh.id, Position: "A", EmployeeID: sh.emp,
			Start: dayTime(day, 8, 0), End: dayTime(day, 16, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
	req, err := svc1.CreateSwapRequest([]Rotation{
		{"s1", "carol"}, {"s3", "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc1.Agree(req.ID, "alice"); err != nil { // 只收一票就“崩溃”
		t.Fatal(err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启重放：pending 申请、已收集的同意、冻结快照都必须还原。
	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	svc2, err := NewService(store2)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := svc2.GetSwapRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != SwapPending || len(pending.Agreed) != 1 || pending.Agreed[0] != "alice" {
		t.Fatalf("replayed pending request = %s agreed=%v", pending.Status, pending.Agreed)
	}
	if len(pending.Snapshots) != 2 || pending.Snapshots[0].Shift.Version != 1 {
		t.Fatalf("replayed snapshots = %+v, want 2 frozen at v1", pending.Snapshots)
	}

	// 重放后继续推进：最后一票基于恢复快照裁决并一次性落班。
	res, err := svc2.Agree(req.ID, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if res.Request.Status != SwapCompleted {
		t.Fatalf("status after restart = %s (%s)", res.Request.Status, res.Request.FailReason)
	}
	for id, want := range map[string]string{"s1": "carol", "s3": "alice"} {
		sh, err := svc2.GetShift(id)
		if err != nil {
			t.Fatal(err)
		}
		if sh.EmployeeID != want || sh.Version != 2 {
			t.Fatalf("%s = %s v%d, want %s v2", id, sh.EmployeeID, sh.Version, want)
		}
	}
}

// TestMemoryStore_ConcurrentAppend 并发追加下事件不丢失、Seq 唯一。
func TestMemoryStore_ConcurrentAppend(t *testing.T) {
	store := NewMemoryStore()
	const writers = 16
	const perWriter = 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				// Service 串行化 Seq 分配；这里直接测 Store 批量追加的可见性。
				evs := []Event{{
					Seq:       int64(w*perWriter + i + 1),
					Type:      evEmployeeRegistered,
					Payload:   map[string]any{},
					Timestamp: dayTime(1, 0, 0),
				}}
				if err := store.Append(evs); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	count := 0
	seen := map[int64]bool{}
	if err := store.Load(func(ev Event) error {
		if seen[ev.Seq] {
			t.Errorf("duplicate seq %d", ev.Seq)
		}
		seen[ev.Seq] = true
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != writers*perWriter {
		t.Fatalf("events = %d, want %d", count, writers*perWriter)
	}
}
