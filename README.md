# go-shift-exchange

多人排班场景下的**原子换班申请**库。一份申请可描述 2~4 名员工之间的班次轮换，
所有参与者一致同意后换班一次性生效；任何人拒绝或撤回，申请立即结束、原排班保持
不变。全部状态以追加事件的方式持久化，重放事件日志即可重建系统。

## 核心保证

1. **整体校验，而非逐条检查原班次**
   换班生效前，会构造“换班后的完整排班”，以员工为维度重新校验：
   - 岗位资质：员工必须持有所排每个岗位的资质；
   - 时间不重叠：同一员工的任意两个班次时间区间不得相交（端点相接不算重叠）；
   - 最短休息间隔：同一员工相邻班次之间至少间隔 `minRest`（默认 8 小时，
     可用 `WithMinRest` 调整，间隔恰好等于阈值合法）。

   因此即使每条轮换单看都合理，交换后产生的新冲突（重叠、休息不足）也会被拦截。
   申请创建时做一次预检，最后一次同意生效时基于最新状态**重新完整复核**。

2. **全员同意 + 一次性原子生效**
   只有集齐全部参与者同意才落班。落班时所有涉及班次在**同一个临界区、同一个
   持久化事件**内完成改派与版本递增：要么全部交换，要么一个都不动。不存在
   “只交换了部分员工”的中间结果，进程崩溃后重放日志也一致。

3. **拒绝 / 撤回立即终结，原排班不变**
   任一方 `Reject`，或已同意方 `Withdraw`，申请立即进入终态
   （`rejected` / `withdrawn`），尚未生效的排班完全不变。

4. **乐观版本控制：旧申请不能覆盖新排班**
   创建申请时冻结涉及班次的当前版本（快照）。若班次在此期间被另一份申请先改动，
   旧申请即使收齐同意，也会在生效裁决时因**版本/持有人不一致**而失败
   （`failed` 终态，附带原因），新排班不会被覆盖。

5. **重复同意幂等**
   同一参与者重复 `Agree` 返回稳定结果（`ActionResult.Idempotent=true`），
   不会重复记录同意、重复推进状态或重复递增班次版本。

6. **并发安全**
   所有操作经过同一把互斥锁，把“校验 → 事件追加 → 状态推进”包成原子临界区。
   “最后一次同意”与“撤回/拒绝”即使并发到达，结果也只有一个：
   整环交换完成（全部班次版本同时 +1），或申请终结且排班原样。测试以
   `go test -race` 与 200 轮并发竞争反复验证。

## 状态机

```
                 所有人同意且裁决通过
        pending ───────────────────────► completed
           │
           ├─ 任一人 Reject ───────────► rejected   ┐
           ├─ 已同意者 Withdraw ───────► withdrawn  ├─ 终态（不再接受任何推进）
           └─ 同意收齐但版本冲突/       │            │
              换班后整体复核失败 ──────► failed      ┘
```

参与者集合由轮换班次的**当前持有人**自动确定，轮换必须在持有人之间构成闭环：
每名参与者交出一个班次、恰好接一个班次；申请人数（即不同持有人数）必须为 2~4。

## API 速览

```go
svc, _ := gx.NewService(store, gx.WithMinRest(8*time.Hour)) // 重放历史事件

// 员工与资质
svc.RegisterEmployee(id, name string, positions []string) (*Employee, error)
svc.AddQualification(employeeID string, positions ...string) error
svc.GetEmployee(id string) (*Employee, error)
svc.ListEmployees() []Employee

// 班次安排（立即按完整排班校验）
svc.ScheduleShift(Shift{ID, Position, Start, End, EmployeeID}) (*Shift, error)
svc.GetShift(id string) (*Shift, error)
svc.ListShifts() []Shift

// 换班申请：提交即冻结快照
svc.CreateSwapRequest([]Rotation{
    {ShiftID: "s1", NewEmployeeID: "carol"},
    {ShiftID: "s3", NewEmployeeID: "alice"},
}) (*SwapRequest, error)
svc.GetSwapRequest(id string) (*SwapRequest, error)
svc.ListSwapRequests() []SwapRequest

// 全员同意后最后一次 Agree 触发原子裁决与落班
svc.Agree(requestID, employeeID string)    (*ActionResult, error)
svc.Reject(requestID, employeeID string)   (*ActionResult, error)
svc.Withdraw(requestID, employeeID string) (*ActionResult, error)

// 历史查询：申请快照、最终变更随记录持久化
svc.HistoryForEmployee(employeeID string) (*EmployeeHistory, error)
svc.ListChanges() []ShiftChange
```

错误可用 `errors.Is` 判断：`ErrNotFound`、`ErrAlreadyExists`、`ErrInvalidInput`
（排班违规可进一步 `errors.As` 为 `*ValidationError`，内含全部违规项）、
`ErrNotParticipant`；对终态申请的推进会返回 `*TerminalStateError`。

## 使用示例

```go
svc, _ := gx.NewService(gx.NewMemoryStore())
svc.RegisterEmployee("alice", "Alice", []string{"A"})
svc.RegisterEmployee("carol", "Carol", []string{"A"})
svc.ScheduleShift(gx.Shift{ID: "s1", Position: "A", EmployeeID: "alice",
    Start: day1(8), End: day1(16)})
svc.ScheduleShift(gx.Shift{ID: "s3", Position: "A", EmployeeID: "carol",
    Start: day3(8), End: day3(16)})

req, _ := svc.CreateSwapRequest([]gx.Rotation{
    {ShiftID: "s1", NewEmployeeID: "carol"},
    {ShiftID: "s3", NewEmployeeID: "alice"},
}) // 冻结 s1/s3 的 v1 快照，并预检换班后的完整排班

svc.Agree(req.ID, "alice") // 只一人同意：排班不变
res, _ := svc.Agree(req.ID, "carol")
// res.Request.Status == completed；s1 -> carol(v2)，s3 -> alice(v2)，一次性生效
```

可运行的完整示例见 `example_test.go`（`go test -run Example`）。

## 持久化

采用事件溯源（event sourcing），状态只通过追加事件改变：

| 事件 | 含义 |
| --- | --- |
| `employee_registered` / `qualification_added` | 员工与资质登记 |
| `shift_scheduled` | 班次安排 |
| `swap_created` | 换班申请，**载荷含提交时冻结的班次快照** |
| `swap_agreed` | 一次同意；最后一次同意的载荷同时携带 `Finalization`（裁决结果与全部最终变更） |
| `swap_rejected` / `swap_withdrawn` | 拒绝 / 撤回，终结申请 |

提供两种 `Store`：

- `MemoryStore`：进程内存储，适合测试；
- `FileStore`：JSON Lines 文件（每行一个事件），整批编码后统一 `fsync`，
  单调递增的 `seq` 保证重放顺序。重新 `NewService` 会自动重放日志，
  申请快照与最终变更都能完整还原，并可在同一日志上继续追加。

```go
store, _ := gx.NewFileStore("data/events.jsonl")
svc, _ := gx.NewService(store) // 启动即重放
defer store.Close()
```

## 运行测试

```bash
go test ./...            # 全部自动化测试
go test -race ./...      # 含数据竞争检测（并发同意/撤回/拒绝）
go test -cover ./...     # 覆盖率（当前约 87%）
go test -run Example ./...
```

测试覆盖：2/3/4 人轮换一次性生效、换班后整体校验（资质、交换诱导的重叠与休息
不足）、拒绝/撤回终态、重复同意与重复拒绝的幂等、版本冲突失败且不覆盖新排班、
“收齐时才变得不合法”的失败路径、最后同意与撤回/拒绝的并发竞争（多轮 + race）、
JSONL 持久化与重放、历史查询。
