package goshiftexchange

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// state 持久化的全部状态。
type state struct {
	Seq       int64                   `json:"seq"`
	Employees map[string]*Employee    `json:"employees"`
	Shifts    map[string]*Shift       `json:"shifts"`
	Requests  map[string]*SwapRequest `json:"requests"`
	Events    []Event                 `json:"events"`
}

func newState() *state {
	return &state{
		Employees: map[string]*Employee{},
		Shifts:    map[string]*Shift{},
		Requests:  map[string]*SwapRequest{},
	}
}

// Store 持有全部数据并负责持久化。所有导出方法都假定调用方已持有 mu，
// 由 Service 统一加锁，保证状态迁移的原子性。
type Store struct {
	mu    sync.Mutex
	path  string // 空字符串表示仅内存
	state *state
}

// OpenStore 打开（或创建）一个持久化 Store。path 为空时仅使用内存。
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, state: newState()}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read store: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, s.state); err != nil {
		return nil, fmt.Errorf("decode store: %w", err)
	}
	if s.state.Employees == nil {
		s.state.Employees = map[string]*Employee{}
	}
	if s.state.Shifts == nil {
		s.state.Shifts = map[string]*Shift{}
	}
	if s.state.Requests == nil {
		s.state.Requests = map[string]*SwapRequest{}
	}
	return s, nil
}

// nextIDLocked 生成单调递增的 ID，序号随状态持久化，重启后不重复。
func (s *Store) nextIDLocked(prefix string) string {
	s.state.Seq++
	return fmt.Sprintf("%s-%d", prefix, s.state.Seq)
}

// saveLocked 将状态原子写入磁盘（先写临时文件再 rename）。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := ensureDir(s.path); err != nil {
		return fmt.Errorf("prepare store dir: %w", err)
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit store: %w", err)
	}
	return nil
}

// ensureDir 确保存储文件所在目录存在。
func ensureDir(path string) error {
	if path == "" {
		return nil
	}
	return os.MkdirAll(filepath.Dir(path), 0o755)
}
