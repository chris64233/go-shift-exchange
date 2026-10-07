# go-shift-exchange

双人排班场景下的**原子双向换班确认**库。每份申请固定两名员工、两条班次和创建时的
排班版本；两名员工分别确认后，系统在同一个临界区和同一批事件中完成版本核对、
整体校验与双方改派。任一方资质不足、休息不足、时间冲突或岗位人数不足，两条班次
都不会改变。管理员取消、排班调整和迟到确认并发时，最终只会有一个有效结果。

## 核心保证

1. **申请内容不可漂移**
   - 每份申请恰好包含两名员工当前持有的两条班次。
   - 创建时冻结两条班次快照、各自版本和全局 `scheduleVersion`。
   - 任一方班次持有人或班次版本变化，旧申请不会覆盖当前排班。
   - 新增班次等排班调整会推进全局版本，并立即把待确认旧申请标记为 `failed`。

2. **四项条件一起通过，双方才一起改变**
   最后一次确认时会构造换班后的完整排班，并重新检查：
   - 岗位资质：双方都必须具备目标班次岗位资质；
   - 时间冲突：同一员工的班次不能重叠；
   - 休息间隔：同一员工相邻班次至少间隔 `minRest`，默认 8 小时；
   - 岗位人数：满足所有已设置的 `StaffingRequirement`。

   校验失败只记录失败裁决，不先改任何一条班次，因此不会留下
   “甲已换、乙未换”的半成品。

3. **双方分别确认，裁决只发生一次**
   第一次 `Agree` 只记录确认，排班不变；第二次确认才触发原子裁决。同一员工重复
   调用自己的确认会返回同一个结果（`Idempotent=true`），不会重复追加事件或重复
   递增班次版本。申请已被另一方拒绝、管理员取消或因排班调整失效后，未确认方的
   迟到调用返回 `ErrConflict`，并携带当前版本或终态原因。

4. **管理员取消与排班调整并发安全**
   `CancelSwapRequest` 会终结待确认申请且不改变排班；重复取消同一终态返回原结果。
   最后确认、管理员取消、新增班次并发时，服务用同一把互斥锁串行化
   “校验 → 事件追加 → 投影”，所以结果只能是：
   - 换班完整完成，两条班次都更新；或
   - 申请取消/失效，两条班次都保持原样。

5. **不永久占用班次**
   待确认申请会阻止第二份申请重复占用同一班次，避免两个待确认结果竞争同一资源。
   申请完成、失败、拒绝、撤回或取消后占用立即释放；失败裁决不会把临时检查状态
   写进排班。

## 状态机

```text
                 两人确认且四项校验通过
pending ┌──────────────────────────────► completed
        ├─ 任一方 Reject ──────────────► rejected
        ├─ 已确认方 Withdraw ──────────► withdrawn
        ├─ 管理员 CancelSwapRequest ───► cancelled
        ├─ 排班调整导致版本失效 ───────► failed
        └─ 迟到确认发现内容/版本冲突 ──► failed 或 ErrConflict
```

除迟到调用可能直接收到 `ErrConflict` 外，终结后的申请不会再次修改排班。

## API 速览

```go
svc, _ := gx.NewService(store, gx.WithMinRest(8*time.Hour))

svc.RegisterEmployee(id, name string, positions []string) (*gx.Employee, error)
svc.AddQualification(employeeID string, positions ...string) error
svc.ScheduleShift(gx.Shift{ID, Position, Start, End, EmployeeID}) (*gx.Shift, error)

svc.SetStaffingRequirement(gx.StaffingRequirement{
    Position: "A", Start: start, End: end, MinStaff: 2,
})
version := svc.ScheduleVersion()

req, _ := svc.CreateTwoWaySwap("s1", "s3")
req, _ = svc.CreateSwapRequest([]gx.Rotation{
    {ShiftID: "s1"},
    {ShiftID: "s3"},
})

svc.Agree(req.ID, "alice")
result, err := svc.Agree(req.ID, "carol")
svc.Reject(req.ID, "carol")
svc.Withdraw(req.ID, "alice")
svc.CancelSwapRequest(req.ID, "admin-42", "manual review")
```

错误可用 `errors.Is` 判断：

- `ErrInvalidInput`：申请不是两人两班、时间或人数参数不合法；
- `ErrConflict`：班次已被待确认申请占用、申请已终结、迟到确认遇到当前版本冲突；
- `ErrNotFound`：员工、班次或申请不存在；
- `ErrNotParticipant`：操作者不是申请中的两名员工之一。

排班违规可 `errors.As` 为 `*ValidationError`；并发冲突可 `errors.As` 为
`*ConflictError` 获取可展示原因。

## 示例

```go
req, _ := svc.CreateTwoWaySwap("s1", "s3")
_, _ = svc.Agree(req.ID, "alice")
if _, err := svc.Agree(req.ID, "carol"); err != nil {
    // 期间管理员取消或排班调整时，这里得到 ErrConflict。
    panic(err)
}
// s1 -> carol，s3 -> alice；两条班次在同一事件后更新。
```

## 持久化

| 事件 | 含义 |
| --- | --- |
| `schedule_changed` | 全局排班版本推进 |
| `shift_scheduled` | 新增班次 |
| `staffing_requirement_set` | 设置岗位人数要求 |
| `swap_created` | 创建固定两人两班申请，含快照和排班版本 |
| `swap_agreed` | 员工确认；最后一次确认携带完整成功或失败裁决 |
| `swap_rejected` / `swap_withdrawn` | 员工拒绝或撤回 |
| `swap_canceled` | 管理员取消 |
| `swap_invalidated` | 排班调整后旧申请自动失效 |

一批事件由 `Store.Append` 原子写入。成功换班时，版本推进、其他待确认申请失效、
最终确认和两条班次改派处于同一批事件；存储失败或进程崩溃后重放，状态仍保持
全部完成或全部未完成。

## 测试

```bash
go test ./...
go test -race ./...
```

测试覆盖双边原子成功、资质/休息/时间/岗位人数联合校验、重复确认返回原结果、
待确认班次重复占用与终态释放、管理员取消竞争、排班调整后的迟到版本冲突、
失败后两边均不变、300 轮取消/确认竞争和 JSONL 事件重放。
