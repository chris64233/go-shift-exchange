package goshiftexchange

import (
	"sort"
	"time"
)

// Employee 员工及其持有的岗位资质。
type Employee struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Qualifications []string `json:"qualifications"`
}

// HasQualification 判断员工是否持有指定资质。
func (e Employee) HasQualification(q string) bool {
	for _, held := range e.Qualifications {
		if held == q {
			return true
		}
	}
	return false
}

// Shift 一个班次。Version 在每次班次归属发生变化时递增，
// 换班申请在提交时冻结所涉班次的版本，用于提交时的乐观并发控制。
type Shift struct {
	ID                    string    `json:"id"`
	Position              string    `json:"position"`
	RequiredQualification string    `json:"required_qualification"`
	Start                 time.Time `json:"start"`
	End                   time.Time `json:"end"`
	EmployeeID            string    `json:"employee_id"`
	Version               int64     `json:"version"`
}

// Move 一次轮换步骤：把 ShiftID 指向的班次从当前持有人转给 ToEmployeeID。
// FromEmployeeID 在申请提交时由系统冻结，调用方无需填写。
type Move struct {
	ShiftID        string `json:"shift_id"`
	FromEmployeeID string `json:"from_employee_id"`
	ToEmployeeID   string `json:"to_employee_id"`
}

// ShiftSnapshot 提交申请时冻结的班次版本快照。
type ShiftSnapshot struct {
	ShiftID    string `json:"shift_id"`
	Version    int64  `json:"version"`
	EmployeeID string `json:"employee_id"`
}

// RequestStatus 换班申请状态。
type RequestStatus string

const (
	StatusPending   RequestStatus = "pending"   // 等待参与者同意
	StatusCompleted RequestStatus = "completed" // 全员同意，换班已原子生效
	StatusRejected  RequestStatus = "rejected"  // 有参与者拒绝
	StatusWithdrawn RequestStatus = "withdrawn" // 有参与者撤回
	StatusFailed    RequestStatus = "failed"    // 收齐同意但版本过期或校验失败
)

// IsTerminal 判断状态是否为终态。
func (s RequestStatus) IsTerminal() bool {
	return s != StatusPending
}

// SwapRequest 一份换班申请，包含 2-4 名员工之间的轮换关系。
type SwapRequest struct {
	ID        string          `json:"id"`
	Moves     []Move          `json:"moves"`
	Snapshots []ShiftSnapshot `json:"snapshots"`
	Consents  map[string]bool `json:"consents"`
	Status    RequestStatus   `json:"status"`
	Reason    string          `json:"reason,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Participants 返回申请的全部参与者（已排序，结果稳定）。
func (r *SwapRequest) Participants() []string {
	out := make([]string, 0, len(r.Consents))
	for id := range r.Consents {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// EventType 历史事件类型。
type EventType string

const (
	EventRequestCreated EventType = "request_created"
	EventConsented      EventType = "consented"
	EventRejected       EventType = "rejected"
	EventWithdrawn      EventType = "withdrawn"
	EventCompleted      EventType = "completed"
	EventFailed         EventType = "failed"
)

// Event 一条历史记录。申请创建时记录快照，完成时记录最终变更。
type Event struct {
	Type      EventType       `json:"type"`
	RequestID string          `json:"request_id"`
	Actor     string          `json:"actor,omitempty"`
	At        time.Time       `json:"at"`
	Detail    string          `json:"detail,omitempty"`
	Moves     []Move          `json:"moves,omitempty"`
	Snapshots []ShiftSnapshot `json:"snapshots,omitempty"`
}

// RequestFilter 历史查询过滤条件，零值字段不参与过滤。
type RequestFilter struct {
	EmployeeID string
	Status     RequestStatus
}
