package goshiftexchange

import (
	"errors"
	"fmt"
)

// 稳定的哨兵错误，可用 errors.Is 判断。
var (
	// ErrNotFound 表示员工、班次或申请不存在。
	ErrNotFound = errors.New("goshiftexchange: not found")
	// ErrAlreadyExists 表示 ID 已被占用。
	ErrAlreadyExists = errors.New("goshiftexchange: already exists")
	// ErrInvalidInput 表示输入不合法（ID 为空、轮换人数不在 2~4 等）。
	ErrInvalidInput = errors.New("goshiftexchange: invalid input")
	// ErrNotParticipant 表示操作者不是该申请的参与者。
	ErrNotParticipant = errors.New("goshiftexchange: employee is not a participant")
)

// NotFoundError 携带实体类别与 ID，Unwrap 到 ErrNotFound。
type NotFoundError struct {
	Kind string
	ID   string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("goshiftexchange: %s %q not found", e.Kind, e.ID)
}

func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// ValidationError 汇总一批排班/换班校验违规项，Unwrap 到 ErrInvalidInput。
type ValidationError struct {
	Violations []string
}

func (e *ValidationError) Error() string {
	if len(e.Violations) == 1 {
		return "goshiftexchange: validation failed: " + e.Violations[0]
	}
	return fmt.Sprintf("goshiftexchange: validation failed (%d violations): %v",
		len(e.Violations), e.Violations)
}

func (e *ValidationError) Unwrap() error { return ErrInvalidInput }

// validationError 用给定违规项构造错误，无违规时返回 nil。
func validationError(violations []string) error {
	if len(violations) == 0 {
		return nil
	}
	return &ValidationError{Violations: violations}
}

// TerminalStateError 表示对已终结申请执行了会推进状态的操作。
type TerminalStateError struct {
	RequestID string
	Status    SwapStatus
}

func (e *TerminalStateError) Error() string {
	return fmt.Sprintf("goshiftexchange: swap request %q is already %s", e.RequestID, e.Status)
}
