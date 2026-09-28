# go-shift-exchange

多人排班的**原子换班**服务。一份换班申请描述 2–4 名员工之间的班次轮换关系，
只有在全部参与者同意后，换班才作为一次原子操作生效；任何人拒绝或撤回，
申请立即结束，原排班保持不变。

## 功能

- **员工与资质登记**：`RegisterEmployee` 登记员工及其持有的岗位资质。
- **班次安排**：`ScheduleShift` 安排班次，自动校验资质、时间不重叠、最短休息间隔。
- **换班申请**：`CreateSwapRequest` 提交一组轮换 Move（每个 Move 把一个班次转交给另一名员工）。
  - 参与者必须为 2–4 人，且每人都既交出又接收班次（构成闭环轮换）。
  - 提交时**冻结**所涉班次的当前版本快照。
  - 提交时对**换班后的完整排班**做整体校验（资质 / 不重叠 / 最短休息），
    而不是逐条校验原班次——例如员工其他已有班次与新换入班次冲突也会被拒绝。
- **同意**：`Consent` 记录参与者同意。
  - 重复同意返回当前的稳定结果，不重复推进状态。
  - 最后一位参与者同意时，在**同一把锁内**原子地完成全部班次的交接：
    不存在"最后一次同意与撤回并发时只换了一半"的中间状态。
  - 提交前重新校验版本快照与完整排班：若所涉班次已被其他申请修改
    （版本不一致），申请以 `failed` 结束，**不会覆盖**新的排班安排。
- **拒绝 / 撤回**：`Reject` / `Withdraw`，任一参与者均可使申请立即结束，排班不变。
- **历史查询**：`GetRequest` / `ListRequests`（按员工、状态过滤）/ `Events`（事件流）。
- **持久化**：`OpenStore(path)` 将员工、班次、申请快照、最终变更与全部事件
  以 JSON 原子写入磁盘（临时文件 + rename），重启后状态完整恢复；
  `OpenStore("")` 为纯内存模式，便于测试。

## 并发与一致性设计

- `Service` 的所有公开方法共用 `Store` 内的一把互斥锁，状态迁移（收齐同意 → 提交、
  撤回 → 关闭）在临界区内一次完成，因此"最后同意 vs 撤回"只会有一个胜出，
  且结果要么全部交换、要么完全不变。
- 乐观并发控制：申请冻结班次版本号，提交时逐一比对；任何不一致都使申请失败，
  保证后完成的申请不会被旧申请覆盖。

## 使用示例

```go
store, _ := goshiftexchange.OpenStore("data/state.json")
svc := goshiftexchange.NewService(store, 8*time.Hour) // 最短休息 8 小时

svc.RegisterEmployee(goshiftexchange.Employee{ID: "A", Qualifications: []string{"nurse"}})
svc.RegisterEmployee(goshiftexchange.Employee{ID: "B", Qualifications: []string{"nurse"}})
svc.ScheduleShift(goshiftexchange.Shift{ID: "s1", RequiredQualification: "nurse",
    Start: day1_8am, End: day1_4pm, EmployeeID: "A"})
svc.ScheduleShift(goshiftexchange.Shift{ID: "s2", RequiredQualification: "nurse",
    Start: day2_8am, End: day2_4pm, EmployeeID: "B"})

req, _ := svc.CreateSwapRequest([]goshiftexchange.Move{
    {ShiftID: "s1", ToEmployeeID: "B"},
    {ShiftID: "s2", ToEmployeeID: "A"},
})
svc.Consent(req.ID, "A")
final, _ := svc.Consent(req.ID, "B") // 全员同意，原子生效
// final.Status == goshiftexchange.StatusCompleted
```

## 测试

```sh
go test ./...
```

覆盖：完整排班校验（资质 / 重叠 / 休息间隔）、轮换规则（2–4 人闭环）、
原子提交、重复同意幂等、拒绝 / 撤回、版本过期失败、
最后同意与撤回并发（50 轮 goroutine 交错验证无部分交换）、
持久化重启往返。
